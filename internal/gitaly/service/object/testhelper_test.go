package object

import (
	"context"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

// setup starts a Gitaly server with the ObjectService registered. The optional modify function
// can adjust the configuration before the server starts, e.g. to set small chunk sizes.
func setup(tb testing.TB, ctx context.Context, modify func(cfg *config.Cfg)) (config.Cfg, gitalypb.ObjectServiceClient) {
	cfg := testcfg.Build(tb)

	if modify != nil {
		modify(&cfg)
	}

	addr := testserver.RunGitalyServer(tb, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterObjectServiceServer(srv, NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	})
	cfg.SocketPath = addr

	conn, err := client.New(ctx, addr)
	require.NoError(tb, err)
	tb.Cleanup(func() { conn.Close() })

	return cfg, gitalypb.NewObjectServiceClient(conn)
}
