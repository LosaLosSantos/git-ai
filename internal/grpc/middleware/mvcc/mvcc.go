package mvcc

import (
	"context"
	"errors"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	logger "github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/middleware"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc"
	grpchealth "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"
)

// cacheBypassMethods lists the RPCs the interceptor skips before it builds a cache.
//
// The list should stay short. An RPC that runs no Git command still benefits from the
// warm-up, which refreshes the local cache from object storage and leaves it warm for
// later requests. Only methods that delete the repository qualify, because there the
// warm-up races with the deletion and nothing is left to read or publish.
var cacheBypassMethods = map[string]struct{}{
	// RemoveRepository renames the repository away and deletes it from disk while the
	// warm-up is still reading the manifest pointer, the manifest and the artifacts.
	gitalypb.RepositoryService_RemoveRepository_FullMethodName: {},
}

// CacheInterceptor is a middleware that injects an mvcc.Cache into the context
// when the repository is using the mvcc reference backend.
type CacheInterceptor struct {
	cacheFactory *mvcc.CacheFactory
	registry     *protoregistry.Registry
	locator      storage.Locator
	logger       logger.Logger
}

// NewMvccInterceptor returns a new MVCC interceptor. cacheFactory should be
// shared with the other MVCC cache consumers of the process so that they all
// use the same ObjectStorage per storage.
func NewMvccInterceptor(cacheFactory *mvcc.CacheFactory, logger logger.Logger, registry *protoregistry.Registry, locator storage.Locator) *CacheInterceptor {
	return &CacheInterceptor{
		cacheFactory: cacheFactory,
		logger:       logger,
		registry:     registry,
		locator:      locator,
	}
}

// UnaryInterceptor returns a Unary Interceptor
func (m *CacheInterceptor) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		methodInfo, err := m.registry.LookupMethod(info.FullMethod)
		if err != nil {
			if !recognizedNonGitalyMethod(info.FullMethod) {
				m.logger.WithError(err).Warn("mvcc unary interceptor: unable to lookup method")
			}
			return handler(ctx, req)
		}

		if _, ok := cacheBypassMethods[info.FullMethod]; ok {
			return handler(ctx, req)
		}

		targetRepo, err := methodInfo.TargetRepo(req.(proto.Message))
		if err != nil {
			return handler(ctx, req)
		}

		cache, err := m.buildCache(ctx, targetRepo, methodInfo.Operation)
		if err != nil || cache == nil {
			return handler(ctx, req)
		}

		// Cleaning up is registered before preparing, so that the repository cannot stay
		// locked if anything goes wrong in between.
		defer cache.Cleanup()
		cache.Prepare(ctx)
		ctx = mvcc.ContextWithCache(ctx, cache)
		ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)
		res, err := handler(ctx, req)
		if err != nil {
			return nil, err
		}
		if err := cache.Commit(ctx); err != nil {
			return nil, mapCommitError(err)
		}

		return res, nil
	}
}

// StreamInterceptor returns a Stream Interceptor
func (m *CacheInterceptor) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := stream.Context()

		methodInfo, err := m.registry.LookupMethod(info.FullMethod)
		if err != nil {
			if !recognizedNonGitalyMethod(info.FullMethod) {
				m.logger.WithError(err).Warn("mvcc unary interceptor: unable to lookup method")
			}
			return handler(srv, stream)
		}

		if _, ok := cacheBypassMethods[info.FullMethod]; ok {
			return handler(srv, stream)
		}

		req := methodInfo.NewRequest()
		if err := stream.RecvMsg(req); err != nil {
			return handler(srv, middleware.NewPeekedStream(ctx, nil, err, stream))
		}

		targetRepo, err := methodInfo.TargetRepo(req)
		if err != nil {
			return handler(srv, middleware.NewPeekedStream(ctx, req, nil, stream))
		}

		cache, err := m.buildCache(ctx, targetRepo, methodInfo.Operation)
		if err != nil || cache == nil {
			return handler(srv, middleware.NewPeekedStream(ctx, req, nil, stream))
		}

		// Cleaning up is registered before preparing, so that the repository cannot stay
		// locked if anything goes wrong in between.
		defer cache.Cleanup()
		cache.Prepare(ctx)
		ctx = mvcc.ContextWithCache(ctx, cache)
		ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)
		if err := handler(srv, middleware.NewPeekedStream(ctx, req, nil, stream)); err != nil {
			return err
		}
		if err := cache.Commit(ctx); err != nil {
			return mapCommitError(err)
		}

		return nil
	}
}

// mapCommitError translates a Cache.Commit error into an appropriate gRPC status. A
// manifest pointer conflict is reported as Aborted so the client may retry against the
// new base; all other errors are returned as-is.
func mapCommitError(err error) error {
	if errors.Is(err, mvcc.ErrManifestPointerConflict) {
		return structerr.NewAborted("mvcc: %w", err)
	}
	return err
}

func (m *CacheInterceptor) buildCache(ctx context.Context, repo *gitalypb.Repository, op protoregistry.OpType) (*mvcc.LocalCache, error) {
	if !m.isMvccReferenceBackend(ctx, repo) {
		return nil, nil
	}

	if fields := logger.CustomFieldsFromContext(ctx); fields != nil {
		fields.RecordMetadata("mvcc.used", true)
	}

	return m.cacheFactory.NewCache(ctx, repo.GetStorageName(), repo.GetRelativePath(), op == protoregistry.OpAccessor)
}

func (m *CacheInterceptor) isMvccReferenceBackend(ctx context.Context, repo *gitalypb.Repository) bool {
	repoPath, err := m.locator.GetRepoPath(ctx, repo)
	if err != nil {
		return false
	}
	backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
	if err != nil {
		return false
	}
	return git.ReferenceBackendMVCC.Name == backend.Name
}

// recognizedNonGitalyMethod returns true if `fullMethod` is a non-Gitaly method
// that the server still accepts.

func recognizedNonGitalyMethod(fullMethod string) bool {
	return strings.Contains(fullMethod, grpchealth.Health_ServiceDesc.ServiceName)
}
