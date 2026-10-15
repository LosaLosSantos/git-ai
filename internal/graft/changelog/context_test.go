package changelog

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOperationFromMethod(t *testing.T) {
	t.Parallel()

	for method, expected := range map[string]string{
		"/gitaly.OperationService/UserFFBranch":          "ff_branch",
		"/gitaly.OperationService/UserLand":              "land",
		"/gitaly.OperationService/UserCommitFiles":       "commit_files",
		"/gitaly.OperationService/UserMergeBranch":       "merge_branch",
		"/gitaly.OperationService/UserRebaseConfirmable": "rebase_confirmable",
		"/gitaly.SmartHTTPService/PostReceivePack":       "post_receive_pack",
		"/gitaly.RefService/UpdateReferences":            "update_references",
		"/gitaly.OperationService/User":                  "user",
	} {
		require.Equal(t, expected, operationFromMethod(method), method)
	}
}

func TestContextHelpers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	require.False(t, IsRewritten(ctx))
	require.True(t, IsRewritten(WithRewritten(ctx)))
	require.Equal(t, "unknown", OperationFromContext(ctx))
	require.Equal(t, "custom", OperationFromContext(WithOperation(ctx, "custom")))
}
