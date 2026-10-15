package conflicts

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/gitaly/service/commit"
	hookservice "github.com/feder-cr/open_scion/internal/gitaly/service/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/service/repository"
	"github.com/feder-cr/open_scion/internal/gitaly/service/ssh"
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

func setupConflictsService(tb testing.TB, hookManager hook.Manager) (config.Cfg, gitalypb.ConflictsServiceClient) {
	cfg := testcfg.Build(tb)

	serverSocketPath := runConflictsServer(tb, cfg, hookManager)
	cfg.SocketPath = serverSocketPath

	client, conn := NewConflictsClient(tb, serverSocketPath)
	tb.Cleanup(func() { conn.Close() })

	return cfg, client
}

func runConflictsServer(tb testing.TB, cfg config.Cfg, hookManager hook.Manager) string {
	return testserver.RunGitalyServer(tb, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterConflictsServiceServer(srv, NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
		gitalypb.RegisterSSHServiceServer(srv, ssh.NewServer(deps))
		gitalypb.RegisterHookServiceServer(srv, hookservice.NewServer(deps))
		gitalypb.RegisterCommitServiceServer(srv, commit.NewServer(deps))
	}, testserver.WithHookManager(hookManager))
}

func NewConflictsClient(tb testing.TB, serverSocketPath string) (gitalypb.ConflictsServiceClient, *grpc.ClientConn) {
	conn, err := client.New(testhelper.Context(tb), serverSocketPath)
	if err != nil {
		tb.Fatal(err)
	}

	return gitalypb.NewConflictsServiceClient(conn), conn
}
