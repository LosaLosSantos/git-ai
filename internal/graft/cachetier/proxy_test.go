package cachetier

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeAuthRepositoryService records the authorization metadata of each call.
type fakeAuthRepositoryService struct {
	gitalypb.UnimplementedRepositoryServiceServer

	mu   sync.Mutex
	auth []string
}

func (f *fakeAuthRepositoryService) RepositoryExists(ctx context.Context, _ *gitalypb.RepositoryExistsRequest) (*gitalypb.RepositoryExistsResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)

	f.mu.Lock()
	f.auth = append(f.auth, strings.Join(md.Get("authorization"), ","))
	f.mu.Unlock()

	return &gitalypb.RepositoryExistsResponse{Exists: true}, nil
}

func (f *fakeAuthRepositoryService) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auth) == 0 {
		return ""
	}
	return f.auth[len(f.auth)-1]
}

func TestTransparentProxy_UnaryAndStreaming(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/proxy.git")

	repoService := &fakeRepositoryService{}
	operations := &fakeOperationService{}
	upstreamAddr := startGRPC(t, func(srv *grpc.Server) {
		gitalypb.RegisterRepositoryServiceServer(srv, repoService)
		gitalypb.RegisterOperationServiceServer(srv, operations)
	})
	tier := newTierOver(t, upstreamAddr, Config{})
	conn := dialAddr(t, tier.addr)

	t.Run("unary RPC", func(t *testing.T) {
		resp, err := gitalypb.NewRepositoryServiceClient(conn).RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo})
		require.NoError(t, err)
		require.True(t, resp.GetExists())
		require.Equal(t, int64(1), repoService.calls.Load())
	})

	t.Run("client streaming RPC forwards every frame", func(t *testing.T) {
		stream, err := gitalypb.NewOperationServiceClient(conn).UserCommitFiles(ctx)
		require.NoError(t, err)

		require.NoError(t, stream.Send(&gitalypb.UserCommitFilesRequest{
			UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{
				Header: &gitalypb.UserCommitFilesRequestHeader{
					Repository:    repo,
					BranchName:    []byte("main"),
					CommitMessage: []byte("proxied"),
				},
			},
		}))
		require.NoError(t, stream.Send(&gitalypb.UserCommitFilesRequest{
			UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
				Action: &gitalypb.UserCommitFilesAction{},
			},
		}))

		_, err = stream.CloseAndRecv()
		require.NoError(t, err)
		require.Equal(t, int64(2), operations.frames.Load())
	})

	t.Run("health is answered locally and not proxied", func(t *testing.T) {
		before := tier.server.Stats().ProxiedCalls

		resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
		require.NoError(t, err)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
		require.Equal(t, before, tier.server.Stats().ProxiedCalls)
	})

	t.Run("unknown methods fail upstream", func(t *testing.T) {
		err := conn.Invoke(ctx, "/gitaly.DoesNotExist/Nope", &gitalypb.RepositoryExistsRequest{Repository: repo}, &gitalypb.RepositoryExistsResponse{})
		require.Equal(t, codes.Unimplemented, status.Code(err))
	})
}

func TestTransparentProxy_Authorization(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/auth.git")

	repoService := &fakeAuthRepositoryService{}
	upstreamAddr := startGRPC(t, func(srv *grpc.Server) {
		gitalypb.RegisterRepositoryServiceServer(srv, repoService)
	})

	t.Run("cache token is used when the client sends none", func(t *testing.T) {
		tier := newTierOver(t, upstreamAddr, Config{Token: "shared-secret"})
		client := gitalypb.NewRepositoryServiceClient(dialAddr(t, tier.addr))

		_, err := client.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo})
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(repoService.lastAuth(), "Bearer v2."), "got %q", repoService.lastAuth())
	})

	t.Run("client authorization is forwarded verbatim", func(t *testing.T) {
		tier := newTierOver(t, upstreamAddr, Config{Token: "shared-secret"})
		client := gitalypb.NewRepositoryServiceClient(dialAddr(t, tier.addr))

		clientCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer client-token")
		_, err := client.RepositoryExists(clientCtx, &gitalypb.RepositoryExistsRequest{Repository: repo})
		require.NoError(t, err)
		require.Equal(t, "Bearer client-token", repoService.lastAuth())
	})

	t.Run("no token configured forwards nothing", func(t *testing.T) {
		tier := newTierOver(t, upstreamAddr, Config{})
		client := gitalypb.NewRepositoryServiceClient(dialAddr(t, tier.addr))

		_, err := client.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo})
		require.NoError(t, err)
		require.Empty(t, repoService.lastAuth())
	})
}
