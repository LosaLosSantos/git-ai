package praefect

import (
	"fmt"

	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/praefect/datastore"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"google.golang.org/grpc"
)

// RepositoryExistsHandler handles /gitaly.RepositoryService/RepositoryExists calls by checking
// whether there is a record of the repository in the database.
func RepositoryExistsHandler(rs datastore.RepositoryStore) grpc.StreamHandler {
	return func(srv interface{}, stream grpc.ServerStream) error {
		var req gitalypb.RepositoryExistsRequest
		if err := stream.RecvMsg(&req); err != nil {
			return fmt.Errorf("receive request: %w", err)
		}

		repo := req.GetRepository()
		if repo == nil {
			return structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet)
		}

		storageName := repo.GetStorageName()
		if storageName == "" {
			return structerr.NewInvalidArgument("%w", storage.ErrStorageNotSet)
		}

		relativePath := repo.GetRelativePath()
		if relativePath == "" {
			return structerr.NewInvalidArgument("%w", storage.ErrRepositoryPathNotSet)
		}

		exists, err := rs.RepositoryExists(stream.Context(), storageName, relativePath)
		if err != nil {
			return fmt.Errorf("repository exists: %w", err)
		}

		if err := stream.SendMsg(&gitalypb.RepositoryExistsResponse{Exists: exists}); err != nil {
			return fmt.Errorf("send response: %w", err)
		}

		return nil
	}
}
