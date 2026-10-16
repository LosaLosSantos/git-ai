package auth

import (
	"context"
	"testing"
	"time"

	gitalycfgauth "github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestInterceptors_AgentMethodAuthorization checks that callers authenticated
// with an agent token cannot call the administrative RPCs that bypass the
// reference policy, while the shared token can, and that the other RPCs stay
// open to agents.
func TestInterceptors_AgentMethodAuthorization(t *testing.T) {
	t.Parallel()

	now := time.Now()
	conf := gitalycfgauth.Config{Token: sharedSecretTest}
	unary, stream, err := Interceptors(conf, testAgents(), true)
	require.NoError(t, err)

	agentAuth := "Bearer " + agentASecret
	sharedAuth := v2Authorization(sharedSecretTest, now)

	for _, tc := range []struct {
		desc          string
		method        string
		authorization string
		wantCode      codes.Code
	}{
		{desc: "agent may land", method: gitalypb.OperationService_UserLand_FullMethodName, authorization: agentAuth, wantCode: codes.OK},
		{desc: "agent may commit files", method: gitalypb.OperationService_UserCommitFiles_FullMethodName, authorization: agentAuth, wantCode: codes.OK},
		{desc: "agent may create branches", method: gitalypb.OperationService_UserCreateBranch_FullMethodName, authorization: agentAuth, wantCode: codes.OK},
		{desc: "agent may delete branches", method: gitalypb.OperationService_UserDeleteBranch_FullMethodName, authorization: agentAuth, wantCode: codes.OK},
		{desc: "agent may read objects", method: gitalypb.ObjectService_GetObjects_FullMethodName, authorization: agentAuth, wantCode: codes.OK},
		{desc: "agent may read trees", method: gitalypb.ObjectService_GetTree_FullMethodName, authorization: agentAuth, wantCode: codes.OK},
		{desc: "agent may find commits", method: gitalypb.CommitService_FindCommit_FullMethodName, authorization: agentAuth, wantCode: codes.OK},
		{desc: "agent may not write references", method: gitalypb.RepositoryService_WriteRef_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not delete references with RefService", method: gitalypb.RefService_DeleteRefs_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not update references with RefService", method: gitalypb.RefService_UpdateReferences_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not fetch remotes", method: gitalypb.RepositoryService_FetchRemote_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not create repositories", method: gitalypb.RepositoryService_CreateRepository_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not fork repositories", method: gitalypb.RepositoryService_CreateFork_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not create repositories from URLs", method: gitalypb.RepositoryService_CreateRepositoryFromURL_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not create repositories from bundles", method: gitalypb.RepositoryService_CreateRepositoryFromBundle_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not remove repositories", method: gitalypb.RepositoryService_RemoveRepository_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not replicate repositories", method: gitalypb.RepositoryService_ReplicateRepository_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not restore repositories", method: gitalypb.RepositoryService_RestoreRepository_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not set custom hooks", method: gitalypb.RepositoryService_SetCustomHooks_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not restore custom hooks", method: gitalypb.RepositoryService_RestoreCustomHooks_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not create repositories from snapshots", method: gitalypb.RepositoryService_CreateRepositoryFromSnapshot_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "shared token may create repositories from snapshots", method: gitalypb.RepositoryService_CreateRepositoryFromSnapshot_FullMethodName, authorization: sharedAuth, wantCode: codes.OK},
		{desc: "agent may not call hook callbacks", method: gitalypb.HookService_ReferenceTransactionHook_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not call pre-receive", method: gitalypb.HookService_PreReceiveHook_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "agent may not manage object pools", method: gitalypb.ObjectPoolService_DeleteObjectPool_FullMethodName, authorization: agentAuth, wantCode: codes.PermissionDenied},
		{desc: "shared token may remove repositories", method: gitalypb.RepositoryService_RemoveRepository_FullMethodName, authorization: sharedAuth, wantCode: codes.OK},
		{desc: "shared token may call hook callbacks", method: gitalypb.HookService_ReferenceTransactionHook_FullMethodName, authorization: sharedAuth, wantCode: codes.OK},
		{desc: "missing token is still unauthenticated", method: gitalypb.RepositoryService_RemoveRepository_FullMethodName, wantCode: codes.Unauthenticated},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := contextWithAuthorization(t, tc.authorization)

			unaryCalled := false
			_, err := unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: tc.method},
				func(context.Context, any) (any, error) {
					unaryCalled = true
					return nil, nil
				})
			require.Equal(t, tc.wantCode, status.Code(err), "unary error: %v", err)
			require.Equal(t, tc.wantCode == codes.OK, unaryCalled)

			streamCalled := false
			err = stream(nil, fakeServerStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: tc.method},
				func(any, grpc.ServerStream) error {
					streamCalled = true
					return nil
				})
			require.Equal(t, tc.wantCode, status.Code(err), "stream error: %v", err)
			require.Equal(t, tc.wantCode == codes.OK, streamCalled)
		})
	}
}

// TestInterceptors_AgentMethodAuthorizationOnlyWithAgents checks that the
// method restrictions do not apply when no agents are configured or on the
// internal server, where upstream behaviour is unchanged.
func TestInterceptors_AgentMethodAuthorizationOnlyWithAgents(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc     string
		agents   bool
		external bool
	}{
		{desc: "external server without agents", external: true},
		{desc: "internal server with agents", agents: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			agents := testAgents()
			if !tc.agents {
				agents = nil
			}

			unary, _, err := Interceptors(gitalycfgauth.Config{}, agents, tc.external)
			require.NoError(t, err)

			called := false
			_, err = unary(contextWithAuthorization(t, ""), nil,
				&grpc.UnaryServerInfo{FullMethod: gitalypb.HookService_ReferenceTransactionHook_FullMethodName},
				func(context.Context, any) (any, error) {
					called = true
					return nil, nil
				})
			require.NoError(t, err)
			require.True(t, called)
		})
	}
}
