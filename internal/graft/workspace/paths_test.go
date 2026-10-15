package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidatePath(t *testing.T) {
	t.Parallel()

	for _, valid := range []string{"a", "a/b", "a/b.txt", "a/b/", "dir-name/.hidden"} {
		_, err := ValidatePath(valid)
		require.NoError(t, err, valid)
	}
	for _, invalid := range []string{
		"", "/", "/etc/passwd", "../x", "a/../b", "a/./b", "a//b", ".",
		"..", "a\x00b", ".graft/state.json", "x/.git/config",
	} {
		_, err := ValidatePath(invalid)
		require.Error(t, err, invalid)
	}
}

func TestValidatePathRejectsBackslashAndColonOnlyOnWindows(t *testing.T) {
	t.Parallel()

	// Both are ordinary characters of Git paths and of file names outside
	// Windows. Rejecting them everywhere made such repositories impossible to
	// clone or to refresh after a land.
	for _, p := range []string{"a\\b", "C:/x", "a:b.txt", "docs/a:b.txt", "x/y?z", "q\"uote"} {
		_, err := ValidatePath(p)
		if runtime.GOOS == "windows" {
			require.Error(t, err, p)
		} else {
			require.NoError(t, err, p)
		}
	}
}

func TestReservedNamesAreCaseInsensitiveAtAnyDepth(t *testing.T) {
	t.Parallel()

	for _, reserved := range []string{
		".GRAFT/state.json", ".graft/objects/x", ".Git/config", "tools/.GIT/x", "a/.git",
		// A nested metadata directory is reserved too, so that a repository file
		// cannot become the state of a workspace that Find locates.
		"tools/.graft/x.txt", "sub/.graft/state.json", "a/b/.Graft",
	} {
		_, err := ValidatePath(reserved)
		require.Error(t, err, reserved)
		require.True(t, isReservedPath(reserved), reserved)
	}

	for _, allowed := range []string{"a/.graft-notes.md", "graft/x", ".gitignore", "a/.graftignore"} {
		_, err := ValidatePath(allowed)
		require.NoError(t, err, allowed)
		require.False(t, isReservedPath(allowed), allowed)
	}
}

func TestNormalizeSparse(t *testing.T) {
	t.Parallel()

	got, err := normalizeSparse([]string{"b/", "a", "a", " c "})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, got)

	got, err = normalizeSparse([]string{"a", "/"})
	require.NoError(t, err)
	require.Nil(t, got, "a root prefix selects the whole repository")

	_, err = normalizeSparse([]string{"../escape"})
	require.Error(t, err)
}

func TestLocalPathStaysInsideRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	full, err := localPath(root, "a/b.txt")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "a", "b.txt"), full)

	_, err = localPath(root, "../outside")
	require.Error(t, err)
}

func TestLocalPathRejectsSymbolicLinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symbolic links not available: %v", err)
	}

	_, err := localPath(root, "link/secret.txt")
	require.ErrorContains(t, err, "symbolic link")
}
