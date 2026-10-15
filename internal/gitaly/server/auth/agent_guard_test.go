package auth

import (
	"context"
	"io"
	"testing"
	"time"

	gitalycfgauth "github.com/feder-cr/open_scion/internal/gitaly/config/auth"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// guardAgentRPCs allows the SSH RPCs through [graft] agent_rpcs, so that the
// git_config_options guard is what decides on them.
var guardAgentRPCs = []string{
	gitalypb.SSHService_SSHUploadPackWithSidechannel_FullMethodName,
	gitalypb.SSHService_SSHReceivePack_FullMethodName,
}

// scriptedServerStream is a server stream that yields the given messages to
// RecvMsg, one per call, and then io.EOF.
type scriptedServerStream struct {
	grpc.ServerStream

	ctx  context.Context
	msgs []proto.Message
}

func (s *scriptedServerStream) Context() context.Context { return s.ctx }

func (s *scriptedServerStream) RecvMsg(m any) error {
	if len(s.msgs) == 0 {
		return io.EOF
	}

	proto.Reset(m.(proto.Message))
	proto.Merge(m.(proto.Message), s.msgs[0])
	s.msgs = s.msgs[1:]
	return nil
}

// TestAgentRequestGuard checks that an agent may not send git_config_options,
// on a unary request or in any message of a stream, while the shared token may.
// The SSH RPCs are allowed to agents for this test, so the guard decides.
func TestAgentRequestGuard(t *testing.T) {
	t.Parallel()

	unary, stream, err := InterceptorsWithAgentRPCs(gitalycfgauth.Config{Token: sharedSecretTest}, testAgents(), guardAgentRPCs, true)
	require.NoError(t, err)

	options := []string{"core.hooksPath=/var/empty"}

	t.Run("agent may not set options on a unary request", func(t *testing.T) {
		t.Parallel()

		called := false
		_, err := unary(contextWithAuthorization(t, "Bearer "+agentASecret),
			&gitalypb.SSHUploadPackWithSidechannelRequest{GitConfigOptions: options},
			&grpc.UnaryServerInfo{FullMethod: gitalypb.SSHService_SSHUploadPackWithSidechannel_FullMethodName},
			func(context.Context, any) (any, error) {
				called = true
				return nil, nil
			})

		require.Equal(t, codes.PermissionDenied, status.Code(err), "error: %v", err)
		require.False(t, called)
	})

	t.Run("agent may call a unary SSH RPC without options", func(t *testing.T) {
		t.Parallel()

		called := false
		_, err := unary(contextWithAuthorization(t, "Bearer "+agentASecret),
			&gitalypb.SSHUploadPackWithSidechannelRequest{},
			&grpc.UnaryServerInfo{FullMethod: gitalypb.SSHService_SSHUploadPackWithSidechannel_FullMethodName},
			func(context.Context, any) (any, error) {
				called = true
				return nil, nil
			})

		require.NoError(t, err)
		require.True(t, called)
	})

	t.Run("agent may not set options in a later stream message", func(t *testing.T) {
		t.Parallel()

		ctx := contextWithAuthorization(t, "Bearer "+agentBSecret)
		ss := &scriptedServerStream{ctx: ctx, msgs: []proto.Message{
			&gitalypb.SSHReceivePackRequest{Repository: &gitalypb.Repository{RelativePath: "repo.git"}},
			&gitalypb.SSHReceivePackRequest{GitConfigOptions: options},
		}}

		var received int
		err := stream(nil, ss, &grpc.StreamServerInfo{FullMethod: gitalypb.SSHService_SSHReceivePack_FullMethodName},
			func(_ any, ss grpc.ServerStream) error {
				for {
					if err := ss.RecvMsg(&gitalypb.SSHReceivePackRequest{}); err != nil {
						return err
					}
					received++
				}
			})

		require.Equal(t, codes.PermissionDenied, status.Code(err), "error: %v", err)
		require.Equal(t, 1, received, "only the message without options reaches the handler")
	})

	t.Run("agent may stream without options", func(t *testing.T) {
		t.Parallel()

		ctx := contextWithAuthorization(t, "Bearer "+agentASecret)
		ss := &scriptedServerStream{ctx: ctx, msgs: []proto.Message{
			&gitalypb.SSHReceivePackRequest{Repository: &gitalypb.Repository{RelativePath: "repo.git"}},
		}}

		err := stream(nil, ss, &grpc.StreamServerInfo{FullMethod: gitalypb.SSHService_SSHReceivePack_FullMethodName},
			func(_ any, ss grpc.ServerStream) error {
				for {
					if err := ss.RecvMsg(&gitalypb.SSHReceivePackRequest{}); err != nil {
						if err == io.EOF {
							return nil
						}
						return err
					}
				}
			})

		require.NoError(t, err)
	})

	t.Run("shared token may set options", func(t *testing.T) {
		t.Parallel()

		called := false
		_, err := unary(contextWithAuthorization(t, v2Authorization(sharedSecretTest, time.Now())), &gitalypb.SSHUploadPackWithSidechannelRequest{GitConfigOptions: options},
			&grpc.UnaryServerInfo{FullMethod: gitalypb.SSHService_SSHUploadPackWithSidechannel_FullMethodName},
			func(context.Context, any) (any, error) {
				called = true
				return nil, nil
			})

		require.NoError(t, err)
		require.True(t, called)
	})
}
