package praefect

import (
	"fmt"

	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/praefect/datastore"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"google.golang.org/grpc"
)

// WalkReposHandler implements an interceptor for the WalkRepos RPC, invoked when calling
// through Praefect. Instead of walking the storage directory in the filesystem, this Praefect
// implementation queries the database for all known repositories in the given virtual storage.
// As a consequence, the modification_time parameter can't be populated in the response.
func WalkReposHandler(rs datastore.RepositoryStore) grpc.StreamHandler {
	return func(srv interface{}, stream grpc.ServerStream) error {
		sendRepo := func(relPath string) error {
			return stream.SendMsg(&gitalypb.WalkReposResponse{
				RelativePath: relPath,
			})
		}

		var req gitalypb.WalkReposRequest
		if err := stream.RecvMsg(&req); err != nil {
			return fmt.Errorf("receive request: %w", err)
		}

		if req.GetStorageName() == "" {
			return structerr.NewInvalidArgument("%w", storage.ErrStorageNotSet)
		}

		repos, err := rs.ListRepositoryPaths(stream.Context(), req.GetStorageName())
		if err != nil {
			return structerr.NewInternal("list repository paths: %w", err)
		}

		for _, repo := range repos {
			if err := sendRepo(repo); err != nil {
				return structerr.NewInternal("send repository path: %w", err)
			}
		}

		return nil
	}
}
