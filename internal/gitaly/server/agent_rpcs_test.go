package server

import (
	"context"
	"net"
	"testing"

	"github.com/feder-cr/git-ai/internal/bootstrap/starter"
	"github.com/feder-cr/git-ai/internal/cache"
	gitmvcc "github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/mvcc"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	agentRPCsTestRepositorySize = "/gitaly.RepositoryService/RepositorySize"
	agentRPCsTestAgentToken     = "builder-secret-for-server-test"
)

// newAgentRPCsFactory returns a server factory whose [graft] configuration has
// one agent and the given agent_rpcs.
func newAgentRPCsFactory(t *testing.T, agentRPCs ...string) *GitalyServerFactory {
	t.Helper()

	cfg := testcfg.Build(t)
	cfg.Auth.Token = ""
	cfg.Graft.Agents = []config.GraftAgent{{Name: "builder", Token: agentRPCsTestAgentToken}}
	cfg.Graft.AgentRPCs = agentRPCs

	logger := testhelper.SharedLogger(t)
	locator := config.NewLocator(cfg)
	mvccMiddleware := mvcc.NewMvccInterceptor(gitmvcc.NewCacheFactory(cfg.RuntimeDir, locator), logger, protoregistry.GitalyProtoPreregistered, locator)
	sf := NewGitalyServerFactory(
		cfg,
		logger,
		backchannel.NewRegistry(),
		cache.New(cfg, locator, logger),
		nil,
		nil,
		TransactionMiddleware{},
		mvccMiddleware,
	)
	t.Cleanup(sf.Stop)

	return sf
}

// serveRepositorySize starts an external server of sf with a stand-in for
// RepositorySize, which the interceptors of the server guard as they guard the
// real RPC. It returns the address of the server.
func serveRepositorySize(t *testing.T, sf *GitalyServerFactory) string {
	t.Helper()

	srv, err := sf.CreateExternal(false)
	require.NoError(t, err)
	t.Cleanup(srv.Stop)

	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "gitaly.RepositoryService",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "RepositorySize",
			Handler: func(_ any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				req := &gitalypb.RepositorySizeRequest{}
				if err := dec(req); err != nil {
					return nil, err
				}

				handler := func(context.Context, any) (any, error) {
					return &gitalypb.RepositorySizeResponse{Size: 42}, nil
				}
				if interceptor == nil {
					return handler(ctx, req)
				}

				return interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: agentRPCsTestRepositorySize}, handler)
			},
		}},
	}, struct{}{})

	listener, err := net.Listen(starter.TCP, "localhost:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()

	return listener.Addr().String()
}

// callRepositorySize calls RepositorySize as the agent of the test server.
func callRepositorySize(t *testing.T, addr string) (*gitalypb.RepositorySizeResponse, error) {
	t.Helper()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ctx := metadata.AppendToOutgoingContext(testhelper.Context(t), "authorization", "Bearer "+agentRPCsTestAgentToken)
	resp := &gitalypb.RepositorySizeResponse{}
	err = conn.Invoke(ctx, agentRPCsTestRepositorySize, &gitalypb.RepositorySizeRequest{}, resp)
	return resp, err
}

// TestGitalyServerFactory_AgentRPCs checks that [graft] agent_rpcs reaches the
// interceptors of the external server: a listed RPC is callable by the agent,
// the same RPC is denied without the entry, and an invalid entry fails the
// server at startup.
func TestGitalyServerFactory_AgentRPCs(t *testing.T) {
	t.Parallel()

	t.Run("listed RPC is callable by an agent", func(t *testing.T) {
		t.Parallel()

		addr := serveRepositorySize(t, newAgentRPCsFactory(t, agentRPCsTestRepositorySize))

		resp, err := callRepositorySize(t, addr)
		require.NoError(t, err)
		require.EqualValues(t, 42, resp.GetSize())
	})

	t.Run("RPC is denied to an agent without the entry", func(t *testing.T) {
		t.Parallel()

		addr := serveRepositorySize(t, newAgentRPCsFactory(t))

		_, err := callRepositorySize(t, addr)
		require.Equal(t, codes.PermissionDenied, status.Code(err), "%v", err)
		require.ErrorContains(t, err, "may not call "+agentRPCsTestRepositorySize)
	})

	t.Run("invalid entries fail at startup", func(t *testing.T) {
		t.Parallel()

		for _, entry := range []string{
			"/gitaly.RepositoryService/WriteRef",
			"/gitaly.RemoteService/UpdateRemoteMirror",
			"/gitaly.RemoteService/FindRemoteRepository",
			"/gitaly.RemoteService/FindRemoteRootRef",
			"/gitaly.RepositoryService/NoSuchMethod",
			"RepositorySize",
		} {
			sf := newAgentRPCsFactory(t, entry)

			_, err := sf.CreateExternal(false)
			require.Error(t, err, "entry %q", entry)
			require.ErrorContains(t, err, "agent_rpcs entry 1", "entry %q", entry)
		}
	})
}
