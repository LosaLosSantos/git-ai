package hook

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/gitaly/service/repository"
	"github.com/feder-cr/open_scion/internal/gitaly/transaction"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/internal/transaction/voting"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"google.golang.org/grpc"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

func setupHookService(tb testing.TB) (config.Cfg, gitalypb.HookServiceClient) {
	tb.Helper()

	cfg := testcfg.Build(tb)
	cfg.SocketPath = runHooksServer(tb, cfg, nil)
	client, conn := newHooksClient(tb, cfg.SocketPath)
	tb.Cleanup(func() { conn.Close() })

	return cfg, client
}

func newHooksClient(tb testing.TB, serverSocketPath string) (gitalypb.HookServiceClient, *grpc.ClientConn) {
	tb.Helper()

	conn, err := client.New(testhelper.Context(tb), serverSocketPath)
	if err != nil {
		tb.Fatal(err)
	}

	return gitalypb.NewHookServiceClient(conn), conn
}

type serverOption func(*server)

func runHooksServer(tb testing.TB, cfg config.Cfg, opts []serverOption, serverOpts ...testserver.GitalyServerOpt) string {
	tb.Helper()

	serverOpts = append(serverOpts, testserver.WithDisablePraefect())

	return testserver.RunGitalyServer(tb, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		hookServer := NewServer(deps)
		for _, opt := range opts {
			opt(hookServer.(*server))
		}

		gitalypb.RegisterHookServiceServer(srv, hookServer)
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	}, serverOpts...)
}

func synchronizedVote(hook string) transaction.PhasedVote {
	return transaction.PhasedVote{
		Vote:  voting.VoteFromData([]byte("synchronize " + hook + " hook")),
		Phase: voting.Synchronized,
	}
}
