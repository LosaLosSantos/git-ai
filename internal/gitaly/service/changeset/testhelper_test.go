package changeset

import (
	"context"
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/gitaly/service/blob"
	"github.com/feder-cr/open_scion/internal/gitaly/service/commit"
	"github.com/feder-cr/open_scion/internal/gitaly/service/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/service/operations"
	"github.com/feder-cr/open_scion/internal/gitaly/service/ref"
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

// testEnv bundles the clients used by the ChangesetService tests.
type testEnv struct {
	ctx        context.Context
	cfg        config.Cfg
	changesets gitalypb.ChangesetServiceClient
	operations gitalypb.OperationServiceClient
	hooks      gitalypb.HookServiceClient
	refs       gitalypb.RefServiceClient
	repository gitalypb.RepositoryServiceClient
	// direct is the server implementation itself, for tests that call its
	// methods without a gRPC client, such as with a fake stream.
	direct gitalypb.ChangesetServiceServer
}

// setupChangesetService starts a Gitaly server with the ChangesetService, and the
// services needed to perform reference updates through hooks, the same way
// the operations tests do.
func setupChangesetService(tb testing.TB, ctx context.Context, cfg config.Cfg) testEnv {
	tb.Helper()

	return setupChangesetServiceWith(tb, ctx, cfg)
}

// setupChangesetServiceWith is setupChangesetService with extra server options,
// such as the storage core of the authoritative tests.
func setupChangesetServiceWith(tb testing.TB, ctx context.Context, cfg config.Cfg, opts ...testserver.GitalyServerOpt) testEnv {
	tb.Helper()

	testcfg.BuildGitalySSH(tb, cfg)
	testcfg.BuildGitalyHooks(tb, cfg)

	var direct gitalypb.ChangesetServiceServer
	serverSocketPath := testserver.RunGitalyServer(tb, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		direct = NewServer(deps)
		gitalypb.RegisterChangesetServiceServer(srv, direct)
		gitalypb.RegisterOperationServiceServer(srv, operations.NewServer(deps))
		gitalypb.RegisterHookServiceServer(srv, hook.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
		gitalypb.RegisterRefServiceServer(srv, ref.NewServer(deps))
		gitalypb.RegisterCommitServiceServer(srv, commit.NewServer(deps))
		gitalypb.RegisterBlobServiceServer(srv, blob.NewServer(deps))
		gitalypb.RegisterSSHServiceServer(srv, ssh.NewServer(deps))
	}, opts...)
	cfg.SocketPath = serverSocketPath

	conn, err := client.New(testhelper.Context(tb), serverSocketPath)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { conn.Close() })

	md := testcfg.GitalyServersMetadataFromCfg(tb, cfg)
	ctx = testhelper.MergeOutgoingMetadata(ctx, md)

	return testEnv{
		ctx:        ctx,
		cfg:        cfg,
		changesets: gitalypb.NewChangesetServiceClient(conn),
		operations: gitalypb.NewOperationServiceClient(conn),
		hooks:      gitalypb.NewHookServiceClient(conn),
		refs:       gitalypb.NewRefServiceClient(conn),
		repository: gitalypb.NewRepositoryServiceClient(conn),
		direct:     direct,
	}
}
