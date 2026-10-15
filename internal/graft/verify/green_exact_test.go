package verify

import (
	"testing"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
	"github.com/stretchr/testify/require"
)

// commitBranch commits path on a new branch named branch, started from main,
// and returns the commit.
func commitBranch(t *testing.T, env *realEnv, branch, path string) string {
	t.Helper()
	res, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
		Branch:   branch,
		StartRev: "main",
		Message:  "add " + path,
		Author:   sdk.Signature{Name: "dev", Email: "dev@example.com"},
		User:     sdk.User{ID: "dev", Username: "dev", Name: "dev", Email: "dev@example.com"},
		Actions:  []sdk.Action{sdk.Put{Path: path, Content: []byte("impostor")}},
	})
	require.NoError(t, err)
	return res.CommitID
}

// TestRealServerGreenRefIsReadByExactName checks that a branch named like the
// green ref, refs/heads/refs/graft/green/main, is not taken for the green ref
// before the green ref exists: it would become the bisection baseline and the
// expected old value of every move, so the branch could never go green.
func TestRealServerGreenRefIsReadByExactName(t *testing.T) {
	env := startRealServer(t)
	tip := env.land("a.txt", "one")
	impostor := commitBranch(t, env, env.cfg.GreenRef, "x.txt")
	require.NotEqual(t, tip, impostor)

	v, closeVerifier := env.verifier()
	defer closeVerifier()
	require.Empty(t, v.Green(), "a branch named like the green ref is not the green ref")

	out, ran, err := v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.True(t, out.Passed, out.Output)
	require.True(t, out.GreenMoved)
	require.Equal(t, tip, v.Green())

	greens, err := dialRefUpdater(env.ctx, env.cfg.Address, env.cfg.Token, env.repo, env.cfg.Storage)
	require.NoError(t, err)
	defer func() { _ = greens.Close() }()

	current, err := greens.Current(env.ctx, env.cfg.GreenRef)
	require.NoError(t, err)
	require.Equal(t, tip, current)

	impostorRef, err := greens.lookupRef(env.ctx, "refs/heads/"+env.cfg.GreenRef)
	require.NoError(t, err)
	require.Equal(t, impostor, impostorRef, "the impostor branch is untouched")
}

// TestRealServerDeletedBranchIsNotResolvedToImpostor checks that once the
// verified branch is deleted, a branch named refs/heads/main
// (refs/heads/refs/heads/main) is not verified in its place.
func TestRealServerDeletedBranchIsNotResolvedToImpostor(t *testing.T) {
	env := startRealServer(t)
	tip := env.land("a.txt", "one")

	v, closeVerifier := env.verifier()
	defer closeVerifier()
	out, _, err := v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, out.GreenMoved)

	commitBranch(t, env, "refs/heads/main", "impostor.txt")

	greens, err := dialRefUpdater(env.ctx, env.cfg.Address, env.cfg.Token, env.repo, env.cfg.Storage)
	require.NoError(t, err)
	defer func() { _ = greens.Close() }()
	_, err = gitalypb.NewOperationServiceClient(greens.conn).UserDeleteBranch(env.ctx, &gitalypb.UserDeleteBranchRequest{
		Repository: greens.repository,
		BranchName: []byte("main"),
		User:       &gitalypb.User{GlId: "dev", GlUsername: "dev", Name: []byte("dev"), Email: []byte("dev@example.com")},
	})
	require.NoError(t, err)

	out, ran, err := v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, "branch does not exist", out.Skipped)
	require.Empty(t, out.Tip)
	require.Equal(t, tip, v.Green())
}

func TestIsFullRefName(t *testing.T) {
	t.Parallel()
	for rev, want := range map[string]bool{
		"refs/heads/main":          true,
		"refs/graft/green/main":    true,
		"refs/heads/a@b":           true,
		"main":                     false,
		"refs/heads/main^{commit}": false,
		"refs/heads/main~1":        false,
		"refs/heads/main@{1}":      false,
		"refs/heads/*":             false,
		"refs/heads/a..b":          false,
		"0123456789012345678901234567890123456789": false,
	} {
		require.Equal(t, want, isFullRefName(rev), rev)
	}
}
