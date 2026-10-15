package praefect

import (
	"context"
	"fmt"

	"github.com/feder-cr/open_scion/internal/git/objectpool"
	objectpoolsvc "github.com/feder-cr/open_scion/internal/gitaly/service/objectpool"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/praefect/datastore"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// DeleteObjectPoolHandler intercepts DeleteObjectPool calls, deletes the database records and
// deletes the object pool from every backing Gitaly node.
func DeleteObjectPoolHandler(rs datastore.RepositoryStore, logger log.Logger, conns Connections) grpc.StreamHandler {
	return removeRepositoryHandler(rs, logger, conns,
		func(stream grpc.ServerStream) (*gitalypb.Repository, error) {
			var req gitalypb.DeleteObjectPoolRequest
			if err := stream.RecvMsg(&req); err != nil {
				return nil, fmt.Errorf("receive request: %w", err)
			}

			repo, err := objectpoolsvc.ExtractPool(&req)
			if err != nil {
				return nil, err
			}

			if !storage.IsRailsPoolRepository(repo) {
				return nil, structerr.NewInvalidArgument("%w", objectpool.ErrInvalidPoolDir)
			}

			return repo, nil
		},
		func(ctx context.Context, conn *grpc.ClientConn, rewritten *gitalypb.Repository) error {
			_, err := gitalypb.NewObjectPoolServiceClient(conn).DeleteObjectPool(ctx, &gitalypb.DeleteObjectPoolRequest{
				ObjectPool: &gitalypb.ObjectPool{
					Repository: rewritten,
				},
			})
			return err
		},
		func() proto.Message { return &gitalypb.DeleteObjectPoolResponse{} },
		false,
	)
}
