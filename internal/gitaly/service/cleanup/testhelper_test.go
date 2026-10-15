package cleanup

import (
	"context"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	hookservice "github.com/feder-cr/open_scion/internal/gitaly/service/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/service/objectpool"
	"github.com/feder-cr/open_scion/internal/gitaly/service/repository"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"google.golang.org/grpc"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

func setupCleanupService(t *testing.T, ctx context.Context) (config.Cfg, *gitalypb.Repository, string, gitalypb.CleanupServiceClient) {
	cfg := testcfg.Build(t)
	cfg.SocketPath = runCleanupServiceServer(t, cfg)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

	client, conn := newCleanupServiceClient(t, cfg.SocketPath)
	t.Cleanup(func() { conn.Close() })

	return cfg, repo, repoPath, client
}

func runCleanupServiceServer(t *testing.T, cfg config.Cfg) string {
	return testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterCleanupServiceServer(srv, NewServer(deps))
		gitalypb.RegisterHookServiceServer(srv, hookservice.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
		gitalypb.RegisterObjectPoolServiceServer(srv, objectpool.NewServer(deps))
	})
}

func newCleanupServiceClient(t *testing.T, serverSocketPath string) (gitalypb.CleanupServiceClient, *grpc.ClientConn) {
	conn, err := client.New(testhelper.Context(t), serverSocketPath)
	if err != nil {
		t.Fatal(err)
	}

	return gitalypb.NewCleanupServiceClient(conn), conn
}
