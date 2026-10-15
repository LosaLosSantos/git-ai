package cachetier

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitalyauth "github.com/feder-cr/open_scion/auth"
	"github.com/feder-cr/open_scion/client"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/config/auth"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	objectservice "github.com/feder-cr/open_scion/internal/gitaly/service/object"
	"github.com/feder-cr/open_scion/internal/gitaly/service/repository"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// dialUpstreamWithToken dials a real Gitaly for typed calls. The token, if any,
// is sent as the caller's own authorization.
func dialUpstreamWithToken(t *testing.T, ctx context.Context, addr, token string) gitalypb.ObjectServiceClient {
	t.Helper()

	opts := []grpc.DialOption{}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token)))
	}
	conn, err := client.DialContext(ctx, addr, client.WithGrpcOptions(opts))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return gitalypb.NewObjectServiceClient(conn)
}

// dialTierWithToken dials a cache tier over TCP, with the caller's authorization
// token if one is given.
func dialTierWithToken(t *testing.T, addr, token string) gitalypb.ObjectServiceClient {
	t.Helper()

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token)))
	}
	conn, err := grpc.NewClient("passthrough:///"+addr, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return gitalypb.NewObjectServiceClient(conn)
}

// startRealTier starts a cache tier that proxies to a real upstream.
func startRealTier(t *testing.T, ctx context.Context, upstreamAddr string, cfg Config) (string, *Server) {
	t.Helper()

	upstreamConn, err := DialUpstream(ctx, upstreamAddr, testLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstreamConn.Close() })

	cfg.Logger = testLogger(t)
	server, err := NewServer(upstreamConn, cfg)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := NewGRPCServer(server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	return listener.Addr().String(), server
}

// writeObjectToDirectory writes data as a blob into objectDir, which is a
// quarantine object directory of the repository at repoPath.
func writeObjectToDirectory(t *testing.T, cfg config.Cfg, repoPath, objectDir string, data []byte) string {
	t.Helper()

	out := gittest.ExecOpts(t, cfg, gittest.ExecConfig{
		Stdin: bytes.NewReader(data),
		Env:   []string{"GIT_OBJECT_DIRECTORY=" + objectDir},
	}, "-C", repoPath, "hash-object", "-w", "--stdin")
	return strings.TrimSpace(string(out))
}

// TestRealGitaly_Authorization runs the cache tier in front of a real Gitaly that
// requires authentication. Every cache read must be authorized as the upstream
// would authorize it, whether the content is cached or not.
func TestRealGitaly_Authorization(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{Auth: auth.Config{Token: "secret"}}))

	upstreamAddr := testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterObjectServiceServer(srv, objectservice.NewServer(deps))
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	})

	cfg.SocketPath = upstreamAddr
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	content := []byte("authorized content")
	blobID := gittest.WriteBlob(t, cfg, repoPath, content)
	treeID := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
		{OID: blobID, Mode: "100644", Path: "file.txt"},
	})
	blob, tree := blobID.String(), treeID.String()

	// upstreamErr is what the upstream answers a caller presenting token directly.
	upstreamErr := func(token string) error {
		_, err := rpcObjects(ctx, dialUpstreamWithToken(t, ctx, upstreamAddr, token), repo, blob)
		return err
	}

	t.Run("authorized callers get the content without the tier holding a token", func(t *testing.T) {
		tierAddr, _ := startRealTier(t, ctx, upstreamAddr, Config{Tier: TierDaemon, MaxBytes: 1 << 20})
		valid := dialTierWithToken(t, tierAddr, "secret")

		// The first read fills the cache with the caller's credentials. The second
		// read is served from memory, after the same authorization.
		for i := 0; i < 2; i++ {
			got, err := rpcObjects(ctx, valid, repo, blob)
			require.NoError(t, err)
			require.Equal(t, content, got[blob].data)
		}

		listing, err := rpcTree(ctx, valid, &gitalypb.GetTreeRequest{Repository: repo, TreeOid: tree})
		require.NoError(t, err)
		require.NotEmpty(t, listing)
	})

	t.Run("callers the upstream rejects get the upstream's error, cold and warm", func(t *testing.T) {
		tierAddr, _ := startRealTier(t, ctx, upstreamAddr, Config{Tier: TierDaemon, MaxBytes: 1 << 20})

		// Warm the cache with a valid caller, so that the rejected callers below
		// would otherwise be served from memory.
		_, err := rpcObjects(ctx, dialTierWithToken(t, tierAddr, "secret"), repo, blob)
		require.NoError(t, err)
		_, err = rpcTree(ctx, dialTierWithToken(t, tierAddr, "secret"), &gitalypb.GetTreeRequest{Repository: repo, TreeOid: tree})
		require.NoError(t, err)

		for _, token := range []string{"wrong", ""} {
			want := upstreamErr(token)
			require.Error(t, want, "the upstream must reject token %q", token)

			_, got := rpcObjects(ctx, dialTierWithToken(t, tierAddr, token), repo, blob)
			require.Equal(t, status.Code(want), status.Code(got), "GetObjects with token %q", token)

			_, got = rpcTree(ctx, dialTierWithToken(t, tierAddr, token), &gitalypb.GetTreeRequest{Repository: repo, TreeOid: tree})
			require.Equal(t, status.Code(want), status.Code(got), "GetTree with token %q", token)
		}
	})

	t.Run("the cache token is used only when the caller sends none", func(t *testing.T) {
		tierAddr, _ := startRealTier(t, ctx, upstreamAddr, Config{Tier: TierDaemon, MaxBytes: 1 << 20, Token: "secret"})

		got, err := rpcObjects(ctx, dialTierWithToken(t, tierAddr, ""), repo, blob)
		require.NoError(t, err)
		require.Equal(t, content, got[blob].data)

		// A caller the upstream rejects keeps its rejection, even though the tier
		// holds a valid token.
		want := upstreamErr("wrong")
		require.Error(t, want)
		_, err = rpcObjects(ctx, dialTierWithToken(t, tierAddr, "wrong"), repo, blob)
		require.Equal(t, status.Code(want), status.Code(err))
	})

	t.Run("quarantine objects are never visible to plain readers", func(t *testing.T) {
		tierAddr, _ := startRealTier(t, ctx, upstreamAddr, Config{Tier: TierDaemon, MaxBytes: 1 << 20})
		tierClient := dialTierWithToken(t, tierAddr, "secret")

		secret := []byte("quarantined secret")
		quarantineDir := filepath.Join(repoPath, "objects", "incoming-zz")
		require.NoError(t, os.MkdirAll(quarantineDir, 0o755))
		secretID := writeObjectToDirectory(t, cfg, repoPath, quarantineDir, secret)

		quarantine := &gitalypb.Repository{
			StorageName:                   repo.GetStorageName(),
			RelativePath:                  repo.GetRelativePath(),
			GitObjectDirectory:            "objects/incoming-zz",
			GitAlternateObjectDirectories: []string{"objects"},
		}

		// Read through the quarantine view first, which is the order that leaks.
		got, err := rpcObjects(ctx, tierClient, quarantine, secretID)
		require.NoError(t, err)
		require.Equal(t, secret, got[secretID].data)

		want, err := rpcObjects(ctx, dialUpstreamWithToken(t, ctx, upstreamAddr, "secret"), repo, secretID)
		require.NoError(t, err)
		require.True(t, want[secretID].missing, "the upstream must not see the quarantine object from the plain repository")

		plain, err := rpcObjects(ctx, tierClient, repo, secretID)
		require.NoError(t, err)
		require.Equal(t, want, plain)
	})
}
