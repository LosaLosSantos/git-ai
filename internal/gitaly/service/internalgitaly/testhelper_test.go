package internalgitaly

import (
	"errors"
	"io"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

func setupInternalGitalyService(t *testing.T, cfg config.Cfg, internalService gitalypb.InternalGitalyServer) gitalypb.InternalGitalyClient {
	add := testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterInternalGitalyServer(srv, internalService)
	}, testserver.WithDisablePraefect())
	conn, err := client.New(testhelper.Context(t), add)
	require.NoError(t, err)
	t.Cleanup(func() { testhelper.MustClose(t, conn) })

	return gitalypb.NewInternalGitalyClient(conn)
}

type streamClient[T any] interface {
	Recv() (T, error)
}

func consumeServerStream[T any](t *testing.T, stream streamClient[T]) []T {
	var results []T
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		results = append(results, resp)
	}
	return results
}
