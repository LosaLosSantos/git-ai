package cachetier

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	hookservice "github.com/feder-cr/open_scion/internal/gitaly/service/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/service/objectpool"
	"github.com/feder-cr/open_scion/internal/gitaly/service/repository"
	"github.com/feder-cr/open_scion/internal/gitaly/service/ssh"
	"github.com/feder-cr/open_scion/internal/grpc/sidechannel"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestSidechannelRPCsPassThroughTheTier checks that a client which dials with
// sidechannel support, as gitlab-shell and Workhorse do, can use the tier. Such
// a client opens its connection with a backchannel handshake, and the clone and
// fetch RPCs send their data over sidechannels. Without the tier's backchannel
// and sidechannel handling, every RPC of such a client fails.
func TestSidechannelRPCsPassThroughTheTier(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.BundleURI).Run(t, testSidechannelRPCsPassThroughTheTier)
}

func testSidechannelRPCsPassThroughTheTier(t *testing.T, ctx context.Context) {
	cfg := testcfg.Build(t)
	upstreamAddr := testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterSSHServiceServer(srv, ssh.NewServer(deps))
		gitalypb.RegisterHookServiceServer(srv, hookservice.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
		gitalypb.RegisterObjectPoolServiceServer(srv, objectpool.NewServer(deps))
	})

	cfg.SocketPath = upstreamAddr
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))

	// The tier dials its upstream the way production does, with sidechannel
	// proxying enabled, and serves the same listener setup as graft-cache.
	upstreamConn, err := DialUpstream(ctx, upstreamAddr, testLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstreamConn.Close() })

	server, err := NewServer(upstreamConn, Config{
		Tier:     TierDaemon,
		MaxBytes: 1 << 20,
		Logger:   testLogger(t),
	})
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := NewGRPCServer(server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	registry := sidechannel.NewRegistry()
	conn, err := sidechannel.Dial(ctx, registry, testhelper.SharedLogger(t), "tcp://"+listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	t.Run("unary RPC", func(t *testing.T) {
		resp, err := gitalypb.NewRepositoryServiceClient(conn).RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo})
		require.NoError(t, err)
		require.True(t, resp.GetExists())
	})

	t.Run("clone over a sidechannel", func(t *testing.T) {
		sidechannelCtx, waiter := sidechannel.RegisterSidechannel(ctx, registry, func(clientConn *sidechannel.ClientConn) error {
			gittest.WritePktlinef(t, clientConn, "want %s multi_ack\n", commitID)
			gittest.WritePktlineFlush(t, clientConn)
			gittest.WritePktlineString(t, clientConn, "done\n")
			require.NoError(t, clientConn.CloseWrite())

			// Drain the pack so that the upstream can finish.
			_, err := io.Copy(io.Discard, clientConn)
			return err
		})
		defer testhelper.MustClose(t, waiter)

		resp, err := gitalypb.NewSSHServiceClient(conn).SSHUploadPackWithSidechannel(sidechannelCtx, &gitalypb.SSHUploadPackWithSidechannelRequest{
			Repository: repo,
		})
		require.NoError(t, err)
		// The payload size varies between runs, so only the negotiation is compared.
		stats := resp.GetPackfileNegotiationStatistics()
		require.Equal(t, int64(2), stats.GetPackets())
		require.Equal(t, []string{"multi_ack"}, stats.GetCaps())
		require.Equal(t, int64(1), stats.GetWants())
	})
}
