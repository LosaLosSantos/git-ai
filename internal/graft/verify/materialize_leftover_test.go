package verify

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// A test run that creates a .git directory must not make a later commit fail:
// the tree of the later commit holds no .git directory of its own.
func TestRunOnceGitDirLeftoverIsRemovedForLaterCommit(t *testing.T) {
	fx := leftoverFixture(t, `if [ -e .git ]; then exit 1; fi; mkdir .git; exit 0`)
	fx.commit("a.txt", "one")
	v := fx.verifier()

	out1, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.True(t, out1.Passed)

	c2 := fx.commit("b.txt", "two")
	out2, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, c2, out2.Tip)
	require.True(t, out2.Passed, "the .git directory left by the previous run must not be in the tree")
	require.Equal(t, c2, fx.green())
}

// A .git directory that a test run creates deeper in the tree is a leftover
// too.
func TestRunOnceNestedGitDirLeftoverIsRemovedForLaterCommit(t *testing.T) {
	fx := leftoverFixture(t, `if [ -e sub/.git/marker ]; then exit 1; fi; mkdir -p sub/.git; echo x > sub/.git/marker; exit 0`)
	fx.commit("a.txt", "one")
	v := fx.verifier()

	out1, _, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, out1.Passed)

	c2 := fx.commit("b.txt", "two")
	out2, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, c2, out2.Tip)
	require.True(t, out2.Passed, "sub/.git/marker left by the previous run must not be in the tree")
	require.Equal(t, c2, fx.green())
}

// An empty or untracked directory that a test run creates must not make a later
// commit fail.
func TestRunOnceDirectoryLeftoverIsRemovedForLaterCommit(t *testing.T) {
	fx := leftoverFixture(t, `if [ -d build ]; then exit 1; fi; mkdir build; exit 0`)
	fx.commit("a.txt", "one")
	v := fx.verifier()

	out1, _, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, out1.Passed)

	c2 := fx.commit("b.txt", "two")
	out2, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, c2, out2.Tip)
	require.True(t, out2.Passed, "the build directory left by the previous run must not be in the tree")
	require.Equal(t, c2, fx.green())
}

// A directory that a test run made unreadable must not stop the verifier from
// cleaning up, so that later commits are still tested on their own tree.
func TestRunOnceUnreadableDirectoryLeftoverIsRemovedForLaterCommit(t *testing.T) {
	fx := leftoverFixture(t, `if [ -e locked ]; then exit 1; fi; mkdir locked; echo x > locked/f; chmod 000 locked; exit 0`)
	// The last run leaves the directory unreadable. Make it accessible again, so
	// that the removal of the temporary directory succeeds.
	t.Cleanup(func() { _ = os.Chmod(fx.treeFile("locked"), 0o700) })
	fx.commit("a.txt", "one")
	v := fx.verifier()

	out1, _, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, out1.Passed)

	c2 := fx.commit("b.txt", "two")
	out2, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, c2, out2.Tip)
	require.True(t, out2.Passed, "the locked directory left by the previous run must not be in the tree")
	require.Equal(t, c2, fx.green())
}
