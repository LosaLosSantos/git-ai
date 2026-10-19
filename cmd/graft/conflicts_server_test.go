package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
)

// TestCLILandConflictsAndResolved drives the agent's way through a conflict the server does not
// resolve (it has no resolver here): graft land --resolve --conflicts-dir writes the package, the
// agent edits the file with the markers, and graft land --resolved lands it.
func TestCLILandConflictsAndResolved(t *testing.T) {
	env := startServer(t)
	base := env.seedMain(t, map[string]string{"x.txt": "1\nbase\n3\n", "y.txt": "y\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	// Another writer changes x.txt and y.txt on main; the local change edits x.txt too.
	gittest.WriteCommit(t, env.cfg, env.repoPath,
		gittest.WithTree(env.writeTree(t, map[string]string{"x.txt": "1\ntip\n3\n", "y.txt": "y2\n"})),
		gittest.WithParents(base), gittest.WithBranch("main"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.txt"), []byte("1\nlocal\n3\n"), 0o644))
	code, _, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "local x")
	require.Equal(t, exitOK, code, stderr)

	conflicts := filepath.Join(t.TempDir(), "conflicts")
	code, stdout, stderr := runCLI(t, "land", "-C", dir, "-author", "Dev <dev@example.com>", "-strategy", "merge", "-resolve", "-conflicts-dir", conflicts)
	require.Equal(t, exitError, code, stdout)
	require.Contains(t, stderr, "merge conflict")
	require.Contains(t, stdout, "the land did not resolve 1 conflicting files")
	require.Contains(t, stdout, "x.txt: not_attempted")
	require.Contains(t, stdout, "conflict resolution is not enabled")
	require.Contains(t, stdout, "graft land --resolved "+conflicts)

	// The file to edit holds the merge with git's diff3 markers; the sides are beside it.
	marked, err := os.ReadFile(filepath.Join(conflicts, "x.txt"))
	require.NoError(t, err)
	require.Equal(t, "1\n<<<<<<< ours (the branch tip)\ntip\n||||||| base\nbase\n=======\nlocal\n>>>>>>> theirs (the change)\n3\n", string(marked))
	for side, want := range map[string]string{"base": "1\nbase\n3\n", "ours": "1\ntip\n3\n", "theirs": "1\nlocal\n3\n"} {
		got, err := os.ReadFile(filepath.Join(conflicts, ".graft", "sides", "x.txt."+side))
		require.NoError(t, err)
		require.Equal(t, want, string(got), side)
	}
	require.FileExists(t, filepath.Join(conflicts, ".graft", "conflict.json"))

	// The agent resolves the file and lands the resolution.
	require.NoError(t, os.WriteFile(filepath.Join(conflicts, "x.txt"), []byte("1\ntip and local\n3\n"), 0o644))
	code, stdout, stderr = runCLI(t, "land", "-C", dir, "-author", "Dev <dev@example.com>", "-resolved", conflicts)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "landed at ")
	require.Contains(t, stdout, "(resolved 1 conflicting files: x.txt)")

	code, stdout, _ = runCLI(t, "cat", "-C", dir, "main:x.txt")
	require.Equal(t, exitOK, code)
	require.Equal(t, "1\ntip and local\n3\n", stdout)
	code, stdout, _ = runCLI(t, "cat", "-C", dir, "main:y.txt")
	require.Equal(t, exitOK, code)
	require.Equal(t, "y2\n", stdout, "the other writer's change is kept")
	code, stdout, _ = runCLI(t, "show", "-C", dir, "-json", "main")
	require.Equal(t, exitOK, code)
	var shown struct {
		Trailers map[string][]string `json:"trailers"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &shown), stdout)
	require.Equal(t, []string{"agent"}, shown.Trailers["Graft-Resolved-By"])
	require.Equal(t, []string{"x.txt"}, shown.Trailers["Graft-Resolved-Path"])
	require.NotContains(t, shown.Trailers, "Graft-Checked-By", "no resolver checked it")
}

func TestCLILandConflictFlagsAreChecked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "land", "-C", dir, "-conflicts-dir", "x")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "--conflicts-dir requires --resolve")

	code, _, stderr = runCLI(t, "land", "-C", dir, "-resolved", "x", "-resolve")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "--resolved takes neither")
}
