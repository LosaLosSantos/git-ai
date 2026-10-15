package verify

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// leftoverFixture returns a fixture whose test command is script, run with sh.
// The command records each run in the runs log first, as testScript does.
func leftoverFixture(t *testing.T, script string) *fixture {
	t.Helper()
	return newFixture(t, func(c *Config) {
		c.Test.Command = []string{"sh", "-c", `echo "$GRAFT_VERIFY_COMMIT" >> "$RUNS"` + "\n" + script}
	})
}

// A test run that deletes a tracked file must not make a later commit pass:
// the tree of the later commit holds the file again.
func TestRunOnceDeletedTrackedFileIsRestoredForLaterCommit(t *testing.T) {
	fx := leftoverFixture(t, `if [ -e broken ]; then rm -f broken; exit 1; fi; exit 0`)
	fx.commit("broken", "x")
	v := fx.verifier()

	out1, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.False(t, out1.Passed)
	require.Empty(t, fx.green())

	c2 := fx.commit("a.txt", "one")
	out2, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, c2, out2.Tip)
	require.False(t, out2.Passed, "the tree of c2 holds broken, which the previous run deleted")
	require.False(t, out2.GreenMoved)
	require.Empty(t, fx.green(), "the green ref must not name a commit that contains a failing file")
	require.Len(t, fx.runsLog(), 2)
}

// A test run that modifies a tracked file must not leave the modification for
// later commits.
func TestRunOnceModifiedTrackedFileIsResetForLaterCommit(t *testing.T) {
	fx := leftoverFixture(t, `if grep -q bad flag.txt 2>/dev/null; then echo ok > flag.txt; exit 1; fi; exit 0`)
	fx.commit("flag.txt", "bad")
	v := fx.verifier()

	out1, _, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.False(t, out1.Passed)

	c2 := fx.commit("a.txt", "one")
	out2, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, c2, out2.Tip)
	require.False(t, out2.Passed, "the tree of c2 holds flag.txt=bad, not the value the previous run wrote")
	require.Empty(t, fx.green())
}

// An untracked file left by a passing run must not make a later commit fail.
func TestRunOnceUntrackedLeftoverIsRemovedForLaterCommit(t *testing.T) {
	fx := leftoverFixture(t, `if [ -e stamp ]; then exit 1; fi; touch stamp; exit 0`)
	c1 := fx.commit("a.txt", "one")
	v := fx.verifier()

	out1, _, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, out1.Passed)
	require.Equal(t, c1, fx.green())

	c2 := fx.commit("b.txt", "two")
	out2, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, c2, out2.Tip)
	require.True(t, out2.Passed, "the stamp left by the previous run must not be in the tree")
	require.Equal(t, c2, fx.green())
}

// Testing the same failing commit again must fail again, as a bisect step or a
// flake check does.
func TestTesterRetestOfFailingCommitFailsAgain(t *testing.T) {
	fx := leftoverFixture(t, `if [ -e broken ]; then rm -f broken; exit 1; fi; exit 0`)
	c1 := fx.commit("broken", "x")
	v := fx.verifier()
	ctx := context.Background()

	for i := range 3 {
		res, err := v.Test(ctx, c1)
		require.NoError(t, err)
		require.False(t, res.Passed, "run %d of a deterministic failure must fail", i+1)
	}
	require.Len(t, fx.runsLog(), 3)
}

// The tree is also cleaned when the verifier starts with a tree that a previous
// process left dirty.
func TestNewVerifierCleansTreeLeftByPreviousProcess(t *testing.T) {
	fx := leftoverFixture(t, `if [ -e broken ]; then rm -f broken; exit 1; fi; exit 0`)
	c1 := fx.commit("broken", "x")

	v := fx.verifier()
	res, err := v.Test(context.Background(), c1)
	require.NoError(t, err)
	require.False(t, res.Passed)

	// A new verifier over the same work directory finds the tree without broken.
	v2 := fx.verifier()
	res, err = v2.Test(context.Background(), c1)
	require.NoError(t, err)
	require.False(t, res.Passed, "the tree left by the previous process must be cleaned before the run")
}
