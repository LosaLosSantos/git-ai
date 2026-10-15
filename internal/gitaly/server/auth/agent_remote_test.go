package auth

import (
	"context"
	"testing"
	"time"

	gitalycfgauth "github.com/feder-cr/open_scion/internal/gitaly/config/auth"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// TestInterceptors_AgentCannotUpdateRemoteMirror checks that an agent may not
// push a repository to a remote URL that it chooses, neither by default nor
// through [graft] agent_rpcs, which is refused at startup.
func TestInterceptors_AgentCannotUpdateRemoteMirror(t *testing.T) {
	t.Parallel()

	method := gitalypb.RemoteService_UpdateRemoteMirror_FullMethodName
	conf := gitalycfgauth.Config{Token: sharedSecretTest}

	_, err := newAgentAllowlist([]string{method})
	require.ErrorContains(t, err, "may never be called by an agent")

	unary, _, err := InterceptorsWithAgentRPCs(conf, testAgents(), nil, true)
	require.NoError(t, err)

	_, err = unary(contextWithAuthorization(t, "Bearer "+agentASecret), nil,
		&grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) {
			t.Fatal("the handler must not run for an agent")
			return nil, nil
		})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "%v", err)

	_, _, err = InterceptorsWithAgentRPCs(conf, testAgents(), []string{method}, true)
	require.ErrorContains(t, err, "may never be called by an agent")
}

// TestInterceptors_AgentCannotQueryRemoteURLs checks that the RemoteService
// RPCs that run `git ls-remote` against a URL the caller chooses are never
// allowed for agents: they are not on the built-in allowlist, are denied at
// call time, and a [graft] agent_rpcs entry naming them is refused at startup.
func TestInterceptors_AgentCannotQueryRemoteURLs(t *testing.T) {
	t.Parallel()

	conf := gitalycfgauth.Config{Token: sharedSecretTest}
	unary, _, err := InterceptorsWithAgentRPCs(conf, testAgents(), nil, true)
	require.NoError(t, err)

	for _, method := range []string{
		gitalypb.RemoteService_FindRemoteRepository_FullMethodName,
		gitalypb.RemoteService_FindRemoteRootRef_FullMethodName,
	} {
		require.True(t, agentNeverAllowed(method), "%s", method)

		_, err := newAgentAllowlist([]string{method})
		require.ErrorContains(t, err, "may never be called by an agent", "%s", method)

		_, _, err = InterceptorsWithAgentRPCs(conf, testAgents(), []string{method}, true)
		require.ErrorContains(t, err, "may never be called by an agent", "%s", method)

		_, err = unary(contextWithAuthorization(t, "Bearer "+agentASecret), nil,
			&grpc.UnaryServerInfo{FullMethod: method},
			func(context.Context, any) (any, error) {
				t.Fatalf("the handler of %s must not run for an agent", method)
				return nil, nil
			})
		require.Equal(t, codes.PermissionDenied, status.Code(err), "%s: %v", method, err)
	}
}

// contextWithGitalyServers returns an incoming context with the authorization
// and gitaly-servers metadata naming address for the storage "evil".
func contextWithGitalyServers(t *testing.T, authorization, address string) context.Context {
	t.Helper()

	outgoing, err := storage.InjectGitalyServers(context.Background(), "evil", address, "attacker-token")
	require.NoError(t, err)
	servers, _ := metadata.FromOutgoingContext(outgoing)

	md := metadata.Join(metadata.Pairs("authorization", authorization), servers)
	return metadata.NewIncomingContext(contextWithAuthorization(t, ""), md)
}

// TestInterceptors_AgentCannotSendGitalyServers checks that an agent may not
// send gitaly-servers metadata, on a unary or a streaming RPC of the built-in
// allowlist. The metadata names the address of other Gitaly servers, which
// UserCommitFiles with a start_repository on another storage dials, so it
// would let an agent make the server connect to any address. The shared token
// may still send it, as Gitaly-to-Gitaly calls do.
func TestInterceptors_AgentCannotSendGitalyServers(t *testing.T) {
	t.Parallel()

	unary, stream, err := InterceptorsWithAgentRPCs(gitalycfgauth.Config{Token: sharedSecretTest}, testAgents(), nil, true)
	require.NoError(t, err)

	const address = "tcp://127.0.0.1:1"
	unaryMethod := gitalypb.RepositoryService_RepositoryExists_FullMethodName
	streamInfo := &grpc.StreamServerInfo{FullMethod: gitalypb.OperationService_UserCommitFiles_FullMethodName}

	runUnary := func(ctx context.Context) (bool, error) {
		called := false
		_, err := unary(ctx, &gitalypb.RepositoryExistsRequest{},
			&grpc.UnaryServerInfo{FullMethod: unaryMethod},
			func(context.Context, any) (any, error) {
				called = true
				return nil, nil
			})
		return called, err
	}

	runStream := func(ctx context.Context) (bool, error) {
		called := false
		ss := &scriptedServerStream{ctx: ctx, msgs: []proto.Message{&gitalypb.UserCommitFilesRequest{}}}
		err := stream(nil, ss, streamInfo, func(any, grpc.ServerStream) error {
			called = true
			return nil
		})
		return called, err
	}

	t.Run("agent may not send gitaly-servers on a unary RPC", func(t *testing.T) {
		t.Parallel()

		called, err := runUnary(contextWithGitalyServers(t, "Bearer "+agentASecret, address))
		require.Equal(t, codes.PermissionDenied, status.Code(err), "error: %v", err)
		require.ErrorContains(t, err, storage.GitalyServersMetadataKey)
		require.False(t, called)
	})

	t.Run("agent may not send gitaly-servers on UserCommitFiles", func(t *testing.T) {
		t.Parallel()

		called, err := runStream(contextWithGitalyServers(t, v2Authorization(agentBSecret, time.Now()), address))
		require.Equal(t, codes.PermissionDenied, status.Code(err), "error: %v", err)
		require.False(t, called, "the handler must not run, so nothing is dialed")
	})

	t.Run("agent may call UserCommitFiles without gitaly-servers", func(t *testing.T) {
		t.Parallel()

		called, err := runStream(contextWithAuthorization(t, "Bearer "+agentASecret))
		require.NoError(t, err)
		require.True(t, called)
	})

	t.Run("shared token may send gitaly-servers", func(t *testing.T) {
		t.Parallel()

		called, err := runUnary(contextWithGitalyServers(t, v2Authorization(sharedSecretTest, time.Now()), address))
		require.NoError(t, err)
		require.True(t, called)

		called, err = runStream(contextWithGitalyServers(t, v2Authorization(sharedSecretTest, time.Now()), address))
		require.NoError(t, err)
		require.True(t, called)
	})
}
