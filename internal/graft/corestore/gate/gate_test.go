package gate_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/coretest"
	"github.com/feder-cr/open_scion/internal/graft/corestore/gate"
	"github.com/feder-cr/open_scion/internal/grpc/protoregistry"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// fakeFreshener records every EnsureFresh call (by relative path) and returns err.
type fakeFreshener struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeFreshener) EnsureFresh(_ context.Context, repo storage.Repository) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, repo.GetRelativePath())
	return f.err
}

func (f *fakeFreshener) Applied(context.Context, storage.Repository) (corestore.Watermark, bool, error) {
	return corestore.Watermark{}, false, nil
}

func (f *fakeFreshener) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeStream returns msg from RecvMsg.
type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
	msg proto.Message
}

func (s *fakeStream) Context() context.Context { return s.ctx }

func (s *fakeStream) RecvMsg(m any) error {
	proto.Merge(m.(proto.Message), s.msg)
	return nil
}

const (
	accessorMethod       = "/gitaly.RepositoryService/HasLocalBranches"
	mutatorMethod        = "/gitaly.RepositoryService/ReplicateRepository"
	streamMutatorMethod  = "/gitaly.RepositoryService/CreateRepositoryFromBundle"
	allowedStreamMutator = "/gitaly.RefService/UpdateReferences"
)

// newGate returns a gate whose freshener is fresh, see newGateEnv.
func newGate(t *testing.T, mode corestore.Mode, fresh corestore.Freshener) *gate.Gate {
	t.Helper()
	g, _, _ := newGateEnv(t, mode, fresh)
	return g
}

// newGateEnv returns a gate whose freshener is fresh, with a storage core on a
// new SQLite database and the storage "default" of a new configuration, and
// returns that configuration and core too. The repository factory is not used
// by the paths these tests cover (no repository is registered through it).
func newGateEnv(t *testing.T, mode corestore.Mode, fresh corestore.Freshener) (*gate.Gate, config.Cfg, *corestore.Core) {
	t.Helper()
	cfg := testcfg.Build(t)
	core := coretest.Open(t, coretest.Options{Mode: mode})
	g := gate.New(core, fresh, testhelper.SharedLogger(t), config.NewLocator(cfg), localrepo.Factory{}, protoregistry.GitalyProtoPreregistered)
	return g, cfg, core
}

// untrackedRepo creates a local repository without a watermark, as one
// created outside the storage core.
func untrackedRepo(t *testing.T, ctx context.Context, cfg config.Cfg) (*gitalypb.Repository, string) {
	t.Helper()
	return gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
}

func requireOperation(t *testing.T, fullMethod string, op protoregistry.OpType) {
	t.Helper()
	mi, err := protoregistry.GitalyProtoPreregistered.LookupMethod(fullMethod)
	require.NoError(t, err)
	require.Equal(t, op, mi.Operation, "operation type of %s", fullMethod)
}

func repoAt(relativePath string) *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: "default", RelativePath: relativePath}
}

func TestGateShadowModeCallsHandlerDirectly(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	fresh := &fakeFreshener{}
	g := newGate(t, corestore.ModeShadow, fresh)

	handled := false
	_, err := g.UnaryInterceptor()(ctx, &gitalypb.ReplicateRepositoryRequest{Repository: repoAt("a.git")},
		&grpc.UnaryServerInfo{FullMethod: mutatorMethod}, func(ctx context.Context, req any) (any, error) {
			handled = true
			return &gitalypb.ReplicateRepositoryResponse{}, nil
		})

	require.NoError(t, err, "shadow mode never blocks a mutator")
	require.True(t, handled)
	require.Empty(t, fresh.called(), "shadow mode never catches up")
}

func TestGateBlocksNonAllowlistedMutator(t *testing.T) {
	t.Parallel()

	requireOperation(t, mutatorMethod, protoregistry.OpMutator)

	ctx := testhelper.Context(t)
	fresh := &fakeFreshener{}
	g := newGate(t, corestore.ModeAuthoritative, fresh)

	handled := false
	_, err := g.UnaryInterceptor()(ctx, &gitalypb.ReplicateRepositoryRequest{Repository: repoAt("a.git")},
		&grpc.UnaryServerInfo{FullMethod: mutatorMethod}, func(ctx context.Context, req any) (any, error) {
			handled = true
			return nil, nil
		})

	require.False(t, handled, "a blocked mutator never reaches the handler")
	require.Empty(t, fresh.called())
	testhelper.RequireGrpcError(t, structerr.NewFailedPrecondition("%s is not supported in authoritative storage mode", mutatorMethod), err)
}

func TestGatePassesThroughUncheckedMethods(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc       string
		fullMethod string
		req        any
	}{
		{desc: "hook service", fullMethod: "/gitaly.HookService/ReferenceTransactionHook"},
		{desc: "storage core service", fullMethod: "/gitaly.StorageCoreService/ImportRepository", req: &gitalypb.ImportRepositoryRequest{Repository: repoAt("a.git")}},
		{desc: "unknown method", fullMethod: "/gitaly.Unknown/Method"},
		{desc: "storage-scoped method", fullMethod: "/gitaly.RemoteService/FindRemoteRepository", req: &gitalypb.FindRemoteRepositoryRequest{StorageName: "default"}},
		{desc: "server-scoped method", fullMethod: "/gitaly.ServerService/ServerInfo", req: &gitalypb.ServerInfoRequest{}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			fresh := &fakeFreshener{err: structerr.NewUnavailable("must not be called")}
			g := newGate(t, corestore.ModeAuthoritative, fresh)

			handled := false
			_, err := g.UnaryInterceptor()(ctx, tc.req, &grpc.UnaryServerInfo{FullMethod: tc.fullMethod},
				func(ctx context.Context, req any) (any, error) {
					handled = true
					return nil, nil
				})
			require.NoError(t, err)
			require.True(t, handled)

			handled = false
			err = g.StreamInterceptor()(nil, &fakeStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: tc.fullMethod},
				func(srv any, ss grpc.ServerStream) error {
					handled = true
					return nil
				})
			require.NoError(t, err)
			require.True(t, handled)

			require.Empty(t, fresh.called(), "no catch-up for %s", tc.fullMethod)
		})
	}
}

func TestGateAccessorCatchesUpBeforeHandler(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	fresh := &fakeFreshener{}
	g := newGate(t, corestore.ModeAuthoritative, fresh)

	var seenByHandler []string
	_, err := g.UnaryInterceptor()(ctx, &gitalypb.HasLocalBranchesRequest{Repository: repoAt("b.git")},
		&grpc.UnaryServerInfo{FullMethod: accessorMethod}, func(ctx context.Context, req any) (any, error) {
			seenByHandler = fresh.called()
			return &gitalypb.HasLocalBranchesResponse{}, nil
		})

	require.NoError(t, err)
	require.Equal(t, []string{"b.git"}, seenByHandler, "EnsureFresh runs before the handler")
}

func TestGateCatchUpErrorStopsHandler(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc string
		err  error
	}{
		{desc: "untracked", err: structerr.NewFailedPrecondition("repository is not imported into the storage core")},
		{desc: "database down", err: structerr.NewUnavailable("storage core: get repository")},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			g, cfg, _ := newGateEnv(t, corestore.ModeAuthoritative, &fakeFreshener{err: tc.err})
			repo, path := untrackedRepo(t, ctx, cfg)

			handled := false
			_, err := g.UnaryInterceptor()(ctx, &gitalypb.HasLocalBranchesRequest{Repository: repo},
				&grpc.UnaryServerInfo{FullMethod: accessorMethod}, func(ctx context.Context, req any) (any, error) {
					handled = true
					return nil, nil
				})

			require.False(t, handled)
			require.Equal(t, status.Code(tc.err), status.Code(err))
			require.DirExists(t, path, "an untracked repository is kept")
		})
	}
}

// A repository that exists neither in the database nor locally is answered by
// the handler, as any missing repository.
func TestGateMissingRepositoryReachesHandler(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	fresh := &fakeFreshener{err: structerr.NewNotFound("repository not found")}
	g := newGate(t, corestore.ModeAuthoritative, fresh)

	resp, err := g.UnaryInterceptor()(ctx, &gitalypb.RepositoryExistsRequest{Repository: repoAt("missing.git")},
		&grpc.UnaryServerInfo{FullMethod: "/gitaly.RepositoryService/RepositoryExists"}, func(ctx context.Context, req any) (any, error) {
			return &gitalypb.RepositoryExistsResponse{Exists: false}, nil
		})

	require.NoError(t, err)
	require.False(t, resp.(*gitalypb.RepositoryExistsResponse).GetExists())
	require.Equal(t, []string{"missing.git"}, fresh.called())
}

func TestGateRequestWithoutRepositoryReachesHandler(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	fresh := &fakeFreshener{}
	g := newGate(t, corestore.ModeAuthoritative, fresh)

	_, err := g.UnaryInterceptor()(ctx, &gitalypb.HasLocalBranchesRequest{},
		&grpc.UnaryServerInfo{FullMethod: accessorMethod}, func(ctx context.Context, req any) (any, error) {
			return nil, structerr.NewInvalidArgument("repository not set")
		})

	require.Equal(t, codes.InvalidArgument, status.Code(err), "the handler validates the request")
	require.Empty(t, fresh.called())
}

func TestGateStreamChecksOnFirstRecv(t *testing.T) {
	t.Parallel()

	requireOperation(t, allowedStreamMutator, protoregistry.OpMutator)

	ctx := testhelper.Context(t)
	fresh := &fakeFreshener{}
	g := newGate(t, corestore.ModeAuthoritative, fresh)

	stream := &fakeStream{ctx: ctx, msg: &gitalypb.UpdateReferencesRequest{Repository: repoAt("s.git")}}

	var seenBeforeRecv []string
	err := g.StreamInterceptor()(nil, stream, &grpc.StreamServerInfo{FullMethod: allowedStreamMutator},
		func(srv any, ss grpc.ServerStream) error {
			seenBeforeRecv = fresh.called()
			for i := 0; i < 2; i++ {
				req := &gitalypb.UpdateReferencesRequest{}
				require.NoError(t, ss.RecvMsg(req))
				require.Equal(t, "s.git", req.GetRepository().GetRelativePath())
			}
			return nil
		})

	require.NoError(t, err)
	require.Empty(t, seenBeforeRecv, "nothing is checked before the request is read")
	require.Equal(t, []string{"s.git"}, fresh.called(), "only the first message is checked")
}

func TestGateStreamCatchUpErrorFailsFirstRecv(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	g, cfg, _ := newGateEnv(t, corestore.ModeAuthoritative, &fakeFreshener{err: structerr.NewFailedPrecondition("repository is not imported into the storage core")})
	repo, _ := untrackedRepo(t, ctx, cfg)

	stream := &fakeStream{ctx: ctx, msg: &gitalypb.UpdateReferencesRequest{Repository: repo}}
	err := g.StreamInterceptor()(nil, stream, &grpc.StreamServerInfo{FullMethod: allowedStreamMutator},
		func(srv any, ss grpc.ServerStream) error {
			return ss.RecvMsg(&gitalypb.UpdateReferencesRequest{})
		})

	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestGateStreamBlocksMutator(t *testing.T) {
	t.Parallel()

	requireOperation(t, streamMutatorMethod, protoregistry.OpMutator)

	ctx := testhelper.Context(t)
	fresh := &fakeFreshener{}
	g := newGate(t, corestore.ModeAuthoritative, fresh)

	handled := false
	err := g.StreamInterceptor()(nil, &fakeStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: streamMutatorMethod},
		func(srv any, ss grpc.ServerStream) error {
			handled = true
			return nil
		})

	require.False(t, handled, "a blocked mutator never reaches the handler")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Empty(t, fresh.called(), "a blocked mutator is not caught up")
}

func TestGateCreateRepositoryRejectsOtherBackends(t *testing.T) {
	t.Parallel()

	for _, backend := range []gitalypb.ReferenceBackend{
		gitalypb.ReferenceBackend_REFERENCE_BACKEND_FILES,
		gitalypb.ReferenceBackend_REFERENCE_BACKEND_MVCC,
	} {
		t.Run(backend.String(), func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			fresh := &fakeFreshener{}
			g := newGate(t, corestore.ModeAuthoritative, fresh)

			handled := false
			_, err := g.UnaryInterceptor()(ctx, &gitalypb.CreateRepositoryRequest{Repository: repoAt("new.git"), ReferenceBackend: backend},
				&grpc.UnaryServerInfo{FullMethod: "/gitaly.RepositoryService/CreateRepository"}, func(ctx context.Context, req any) (any, error) {
					handled = true
					return nil, nil
				})

			require.False(t, handled)
			testhelper.RequireGrpcError(t, structerr.NewFailedPrecondition("authoritative storage mode requires a reftable repository"), err)
			require.Empty(t, fresh.called(), "CreateRepository never catches up")
		})
	}
}

// The allowlist only holds mutators of repository scope: anything else in it
// would be dead.
func TestGateAllowlistHoldsRepositoryMutators(t *testing.T) {
	t.Parallel()

	for fullMethod := range gate.AuthoritativeMutators {
		mi, err := protoregistry.GitalyProtoPreregistered.LookupMethod(fullMethod)
		require.NoError(t, err, fullMethod)
		require.Equal(t, protoregistry.OpMutator, mi.Operation, fullMethod)
		require.Equal(t, protoregistry.ScopeRepository, mi.Scope, fullMethod)
	}
}
