package repository

import (
	"context"
	"testing"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/stats"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/commit"
	hookservice "github.com/feder-cr/git-ai/internal/gitaly/service/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/service/objectpool"
	"github.com/feder-cr/git-ai/internal/gitaly/service/ref"
	"github.com/feder-cr/git-ai/internal/gitaly/service/remote"
	"github.com/feder-cr/git-ai/internal/gitaly/service/ssh"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

// requireFunctionalRepository asserts that the repository at repoPath uses the expected reference
// backend and is functional: a commit can be written to it and read back. For the MVCC reference
// backend this exercises the MVCC-enabled Git binary end to end, validating that the repository is
// well-configured after creation.
func requireFunctionalRepository(tb testing.TB, ctx context.Context, cfg config.Cfg, repoPath string, expected git.ReferenceBackend) {
	tb.Helper()

	backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
	require.NoError(tb, err)
	require.Equal(tb, expected.Name, backend.Name)

	commitID := gittest.WriteCommit(tb, cfg, repoPath, gittest.WithBranch("post-create-write"))
	require.Equal(tb, commitID, gittest.ResolveRevision(tb, cfg, repoPath, "refs/heads/post-create-write"))
}

func newRepositoryClient(tb testing.TB, cfg config.Cfg, serverSocketPath string) gitalypb.RepositoryServiceClient {
	connOpts := []grpc.DialOption{
		client.UnaryInterceptor(), client.StreamInterceptor(),
	}
	if cfg.Auth.Token != "" {
		connOpts = append(connOpts, grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(cfg.Auth.Token)))
	}
	conn, err := client.New(testhelper.Context(tb), serverSocketPath, client.WithGrpcOptions(connOpts))
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, conn.Close()) })

	return gitalypb.NewRepositoryServiceClient(conn)
}

func newMuxedRepositoryClient(t *testing.T, ctx context.Context, cfg config.Cfg, serverSocketPath string, handshaker client.Handshaker) gitalypb.RepositoryServiceClient {
	conn, err := client.New(ctx, serverSocketPath,
		client.WithGrpcOptions([]grpc.DialOption{
			grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(cfg.Auth.Token)),
		}),
		client.WithHandshaker(handshaker),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return gitalypb.NewRepositoryServiceClient(conn)
}

func runRepositoryService(tb testing.TB, cfg config.Cfg, opts ...testserver.GitalyServerOpt) (gitalypb.RepositoryServiceClient, string) {
	serverSocketPath := testserver.RunGitalyServer(tb, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterRepositoryServiceServer(srv, NewServer(deps))
		gitalypb.RegisterHookServiceServer(srv, hookservice.NewServer(deps))
		gitalypb.RegisterRemoteServiceServer(srv, remote.NewServer(deps))
		gitalypb.RegisterSSHServiceServer(srv, ssh.NewServer(deps))
		gitalypb.RegisterRefServiceServer(srv, ref.NewServer(deps))
		gitalypb.RegisterCommitServiceServer(srv, commit.NewServer(deps))
		gitalypb.RegisterObjectPoolServiceServer(srv, objectpool.NewServer(deps))
	}, opts...)

	return newRepositoryClient(tb, cfg, serverSocketPath), serverSocketPath
}

func setupRepositoryService(tb testing.TB, opts ...testserver.GitalyServerOpt) (config.Cfg, gitalypb.RepositoryServiceClient) {
	cfg := testcfg.Build(tb)

	testcfg.BuildGitalyHooks(tb, cfg)
	testcfg.BuildGitalySSH(tb, cfg)

	client, serverSocketPath := runRepositoryService(tb, cfg, opts...)
	cfg.SocketPath = serverSocketPath

	return cfg, client
}

func requireRepositoryInfoLog(tb testing.TB, entries ...*logrus.Entry) {
	tb.Helper()

	const key = "repository_info"
	for _, entry := range entries {
		if entry.Message == "repository info" {
			require.Contains(tb, entry.Data, "grpc.request.glProjectPath")
			require.Contains(tb, entry.Data, "grpc.request.glRepository")
			require.Contains(tb, entry.Data, key, "objects info not found")
			require.IsType(tb, stats.RepositoryInfo{}, entry.Data[key])
			return
		}
	}
	require.FailNow(tb, "no info about statistics")
}
