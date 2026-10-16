package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// TestAgentLandLeavesNoTemporaryBranch checks that a commit and land made by an
// agent through the CLI backend leaves no temporary branch behind. The temporary
// branch is deleted with OperationService.UserDeleteBranch, which agents may
// call, and not with RefService.DeleteRefs, which they may not.
func TestAgentLandLeavesNoTemporaryBranch(t *testing.T) {
	t.Parallel()

	env := startPolicyServer(t)
	ctx := env.ctx
	b, err := openSDKBackend(ctx, target{Address: env.addr, Token: "alice-secret", Storage: env.storage, Repo: env.relPath, User: "alice"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	dir := filepath.Join(t.TempDir(), "ws")
	ws, err := workspace.Clone(ctx, dir, b, workspace.CloneOptions{
		Address: env.addr,
		Storage: env.storage,
		Repo:    env.relPath,
		Branch:  "main",
	})
	require.NoError(t, err)

	writeLocal(t, dir, "a.txt", "two\n")
	res, err := ws.Commit(ctx, workspace.CommitRequest{
		Message:    "change a",
		Author:     testAuthor(),
		User:       "alice",
		LandBranch: "main",
	})
	require.NoError(t, err)
	require.True(t, res.Landed)
	require.Contains(t, res.Branch, "graft/alice/")

	require.Equal(t, "two\n", readRemoteFile(t, env, b, "main", "a.txt"))
	_, err = b.ResolveRevision(ctx, res.Branch)
	require.ErrorIs(t, err, workspace.ErrNotFound, "the temporary branch %q must be deleted after the land", res.Branch)

	refs := gittest.Exec(t, env.cfg, "-C", env.repoPath, "for-each-ref", "refs/heads/graft")
	require.Empty(t, string(refs), "no temporary branch may be left behind")
}
