package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCLIAgainstServer drives the graft command line against a real server
// through the default backend. It runs serially because the CLI tests share the
// package-level backend factory.
func TestCLIAgainstServer(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"a/x.txt": "ax\n", "top.txt": "top\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, stdout, stderr := runCLI(t, "clone", "-storage", env.storage, "-sparse", "a", env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "cloned")
	require.NoFileExists(t, filepath.Join(dir, "top.txt"))

	code, stdout, _ = runCLI(t, "status", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Equal(t, "nothing changed\n", stdout)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "x.txt"), []byte("changed\n"), 0o644))

	code, stdout, _ = runCLI(t, "diff", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "-ax\n+changed\n")

	code, stdout, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "change x", "-land", "main")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "and landed it on main")

	code, stdout, _ = runCLI(t, "cat", "-C", dir, "main:a/x.txt")
	require.Equal(t, exitOK, code)
	require.Equal(t, "changed\n", stdout)

	code, stdout, stderr = runCLI(t, "cat", "-C", dir, "main:missing.txt")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "not found")

	code, stdout, _ = runCLI(t, "blame", "-C", dir, "a/x.txt")
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "changed")

	code, stdout, _ = runCLI(t, "history", "-C", dir, "a/x.txt")
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "change x")

	code, stdout, _ = runCLI(t, "log", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "refs/heads/main")

	code, stdout, _ = runCLI(t, "update", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "already up to date")
}

func TestCLIRejectsUnknownStrategyBeforeContactingServer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "land", "-C", dir, "-strategy", "squash")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "unknown strategy")
}
