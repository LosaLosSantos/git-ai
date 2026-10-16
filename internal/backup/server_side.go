package backup

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ServerSideAdapter allows calling the server-side backup RPCs `BackupRepository`
// and `RestoreRepository` through `backup.Strategy` such that server-side
// backups can be used with `backup.Pipeline`. It also implements
// `backup.IDHandler` via dedicated RPCs.
type ServerSideAdapter struct {
	pool *client.Pool
}

// NewServerSideAdapter creates and returns initialized *ServerSideAdapter instance.
func NewServerSideAdapter(pool *client.Pool) *ServerSideAdapter {
	return &ServerSideAdapter{
		pool: pool,
	}
}

// Create calls the BackupRepository RPC.
func (ss ServerSideAdapter) Create(ctx context.Context, req *CreateRequest) error {
	if err := setContextServerInfo(ctx, &req.Server, req.Repository.GetStorageName()); err != nil {
		return fmt.Errorf("server-side create: %w", err)
	}

	client, err := ss.newRepoClient(ctx, req.Server)
	if err != nil {
		return fmt.Errorf("server-side create: %w", err)
	}

	_, err = client.BackupRepository(ctx, &gitalypb.BackupRepositoryRequest{
		Repository:       req.Repository,
		VanityRepository: req.VanityRepository,
		BackupId:         req.BackupID,
		LatestBackupId:   req.LatestBackupID,
		PathPrefix:       req.PathPrefix,
		Incremental:      req.Incremental,
	})
	if err != nil {
		st := status.Convert(err)
		if st.Code() == codes.NotFound {
			return fmt.Errorf("server-side create: %w: %s", ErrSkipped, err.Error())
		}
		for _, detail := range st.Details() {
			switch detail.(type) {
			case *gitalypb.BackupRepositoryResponse_SkippedError:
				return fmt.Errorf("server-side create: %w: %s", ErrSkipped, err.Error())
			}
		}

		return fmt.Errorf("server-side create: %w", err)
	}

	return nil
}

// Restore calls the RestoreRepository RPC.
func (ss ServerSideAdapter) Restore(ctx context.Context, req *RestoreRequest) error {
	if err := setContextServerInfo(ctx, &req.Server, req.Repository.GetStorageName()); err != nil {
		return fmt.Errorf("server-side restore: %w", err)
	}

	client, err := ss.newRepoClient(ctx, req.Server)
	if err != nil {
		return fmt.Errorf("server-side restore: %w", err)
	}

	protoRefBackend, err := git.ReferenceBackendToProto(req.ReferenceBackend)
	if err != nil {
		return fmt.Errorf("server-side restore: %w", err)
	}

	_, err = client.RestoreRepository(ctx, &gitalypb.RestoreRepositoryRequest{
		Repository:       req.Repository,
		VanityRepository: req.VanityRepository,
		AlwaysCreate:     req.AlwaysCreate,
		BackupId:         req.BackupID,
		UseLatest:        req.UseLatest,
		PathPrefix:       req.PathPrefix,
		ReferenceBackend: protoRefBackend,
	})
	if err != nil {
		st := status.Convert(err)
		for _, detail := range st.Details() {
			switch detail.(type) {
			case *gitalypb.RestoreRepositoryResponse_SkippedError:
				return fmt.Errorf("server-side restore: %w: %s", ErrSkipped, err.Error())
			}
		}

		return structerr.New("server-side restore: %w", err)
	}

	return nil
}

// WriteBackupID calls the WriteBackupID RPC on the server of the requested
// storage.
func (ss ServerSideAdapter) WriteBackupID(ctx context.Context, req *WriteBackupIDRequest) error {
	if err := setContextServerInfo(ctx, &req.Server, req.StorageName); err != nil {
		return fmt.Errorf("server-side write backup id: %w", err)
	}

	client, err := ss.newBackupClient(ctx, req.Server)
	if err != nil {
		return fmt.Errorf("server-side write backup id: %w", err)
	}

	_, err = client.WriteBackupID(ctx, &gitalypb.WriteBackupIDRequest{
		StorageName: req.StorageName,
		BackupId:    req.BackupID,
		PathPrefix:  req.PathPrefix,
	})
	if err != nil {
		return structerr.New("server-side write backup id: %w", err)
	}

	return nil
}

// ReadLatestBackupID calls the ReadLatestBackupID RPC. It runs before any
// repository is processed, so it tries the storages in the context's server
// map in sorted order until one of them answers. A storage that fails the RPC
// for any reason other than NotFound is skipped. Examples are a placeholder
// storage that points at a Praefect without a virtual storage of that name, a
// server without a backup sink, and a server that is down. The servers are
// expected to share one backup sink, so any answer is as good as another.
//
// NotFound is final. If the servers don't share a sink after all, a marker
// from another sink could name an older backup run. Callers fall back to
// per-repository lookups, which are correct for any sink.
func (ss ServerSideAdapter) ReadLatestBackupID(ctx context.Context, pathPrefix string) (string, error) {
	servers, err := storage.ExtractGitalyServers(ctx)
	if err != nil {
		return "", fmt.Errorf("server-side read latest backup id: extract gitaly servers: %w", err)
	}

	if len(servers) == 0 {
		return "", fmt.Errorf("server-side read latest backup id: no gitaly servers found in context")
	}

	var errs []error
	for _, storageName := range slices.Sorted(maps.Keys(servers)) {
		backupID, err := ss.readLatestBackupID(ctx, servers[storageName], storageName, pathPrefix)
		switch {
		case err == nil:
			return backupID, nil
		case errors.Is(err, ErrDoesntExist):
			return "", err
		default:
			errs = append(errs, structerr.New("storage %q: %w", storageName, err))
		}
	}

	return "", structerr.New("server-side read latest backup id: %w", errors.Join(errs...))
}

func (ss ServerSideAdapter) readLatestBackupID(ctx context.Context, server storage.ServerInfo, storageName, pathPrefix string) (string, error) {
	client, err := ss.newBackupClient(ctx, server)
	if err != nil {
		return "", err
	}

	response, err := client.ReadLatestBackupID(ctx, &gitalypb.ReadLatestBackupIDRequest{
		StorageName: storageName,
		PathPrefix:  pathPrefix,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", ErrDoesntExist
		}
		return "", err
	}

	return response.GetBackupId(), nil
}

func (ss ServerSideAdapter) newRepoClient(ctx context.Context, server storage.ServerInfo) (gitalypb.RepositoryServiceClient, error) {
	conn, err := ss.pool.Dial(ctx, server.Address, server.Token)
	if err != nil {
		return nil, err
	}

	return gitalypb.NewRepositoryServiceClient(conn), nil
}

func (ss ServerSideAdapter) newBackupClient(ctx context.Context, server storage.ServerInfo) (gitalypb.BackupServiceClient, error) {
	conn, err := ss.pool.Dial(ctx, server.Address, server.Token)
	if err != nil {
		return nil, err
	}

	return gitalypb.NewBackupServiceClient(conn), nil
}
