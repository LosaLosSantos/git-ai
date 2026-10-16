package smarthttp

import (
	"context"
	"testing"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/commit"
	hookservice "github.com/feder-cr/git-ai/internal/gitaly/service/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/service/objectpool"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
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

func startSmartHTTPServerWithOptions(t *testing.T, cfg config.Cfg, opts []ServerOpt, serverOpts []testserver.GitalyServerOpt) testserver.GitalyServer {
	return testserver.StartGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterSmartHTTPServiceServer(srv, NewServer(deps, opts...))
		gitalypb.RegisterCommitServiceServer(srv, commit.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
		gitalypb.RegisterObjectPoolServiceServer(srv, objectpool.NewServer(deps))
		gitalypb.RegisterHookServiceServer(srv, hookservice.NewServer(deps))
	}, serverOpts...)
}

func startSmartHTTPServer(t *testing.T, cfg config.Cfg, opts ...ServerOpt) testserver.GitalyServer {
	return startSmartHTTPServerWithOptions(t, cfg, opts, nil)
}

func runSmartHTTPServer(t *testing.T, cfg config.Cfg, opts ...ServerOpt) string {
	gitalyServer := startSmartHTTPServer(t, cfg, opts...)
	return gitalyServer.Address()
}

func newSmartHTTPClient(t *testing.T, serverSocketPath, token string) gitalypb.SmartHTTPServiceClient {
	t.Helper()

	conn, err := client.New(testhelper.Context(t), serverSocketPath,
		client.WithGrpcOptions([]grpc.DialOption{
			grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token)),
		}))
	require.NoError(t, err)

	t.Cleanup(func() {
		testhelper.MustClose(t, conn)
	})

	return gitalypb.NewSmartHTTPServiceClient(conn)
}

func newMuxedSmartHTTPClient(t *testing.T, ctx context.Context, serverSocketPath, token string, serverFactory backchannel.ServerFactory) gitalypb.SmartHTTPServiceClient {
	t.Helper()

	conn, err := client.New(
		ctx,
		serverSocketPath,
		client.WithGrpcOptions([]grpc.DialOption{grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token))}),
		client.WithHandshaker(backchannel.NewClientHandshaker(testhelper.SharedLogger(t), serverFactory, backchannel.DefaultConfiguration())),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	return gitalypb.NewSmartHTTPServiceClient(conn)
}
