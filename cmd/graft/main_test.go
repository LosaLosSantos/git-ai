package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/stretchr/testify/require"
)

// useMemoryBackend routes every command to mem for the duration of the test.
// The CLI tests share the package-level factory, so they do not run in parallel.
func useMemoryBackend(t *testing.T, mem *workspace.MemoryBackend) {
	t.Helper()
	previous := backendFactory
	backendFactory = func(context.Context, target) (workspace.Backend, error) { return mem, nil }
	t.Cleanup(func() { backendFactory = previous })
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestParseAuthor(t *testing.T) {
	t.Parallel()

	for input, expected := range map[string]workspace.Signature{
		"":                       {Name: "graft", Email: "graft@localhost"},
		"Ada Lovelace <a@x.org>": {Name: "Ada Lovelace", Email: "a@x.org"},
		"bot":                    {Name: "bot", Email: "graft@localhost"},
	} {
		got, err := parseAuthor(input)
		require.NoError(t, err, input)
		require.Equal(t, expected, got, input)
	}

	for _, input := range []string{"<only@email.org>", "Name <broken"} {
		_, err := parseAuthor(input)
		require.Error(t, err, input)
	}
}

func TestRunUsageErrors(t *testing.T) {
	useMemoryBackend(t, workspace.NewMemoryBackend())

	code, _, stderr := runCLI(t)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "usage: graft")

	code, _, stderr = runCLI(t, "bogus")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, `unknown command "bogus"`)

	code, _, _ = runCLI(t, "clone", "only-one")
	require.Equal(t, exitUsage, code)

	code, _, stderr = runCLI(t, "commit")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "-m MESSAGE")

	code, _, _ = runCLI(t, "help")
	require.Equal(t, exitOK, code)
}

func TestRunOutsideWorkspaceFails(t *testing.T) {
	useMemoryBackend(t, workspace.NewMemoryBackend())

	dir := t.TempDir()
	code, _, stderr := runCLI(t, "status", "-C", dir)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "not a graft workspace")
}

func TestRunCloneEditCommitAndRead(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a/x.txt", []byte("one"))
	mem.Put("main", "b/y.txt", []byte("two"))
	useMemoryBackend(t, mem)

	dir := filepath.Join(t.TempDir(), "ws")
	code, stdout, stderr := runCLI(t, "clone", "-sparse", "a", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "cloned repo (main)")
	require.NoDirExists(t, filepath.Join(dir, "b"))

	code, stdout, _ = runCLI(t, "status", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Equal(t, "nothing changed\n", stdout)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "x.txt"), []byte("changed"), 0o644))

	code, stdout, _ = runCLI(t, "diff", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "-one\n+changed\n")

	code, stdout, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "change x", "-land", "main")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "and landed it on main")

	landed, err := mem.ReadFile(context.Background(), "main", "a/x.txt")
	require.NoError(t, err)
	require.Equal(t, "changed", string(landed))

	code, stdout, _ = runCLI(t, "log", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "refs/heads/main")
	require.Contains(t, stdout, "land")

	code, stdout, _ = runCLI(t, "cat", "-C", dir, "main:a/x.txt")
	require.Equal(t, exitOK, code)
	require.Equal(t, "changed", stdout)

	code, stdout, _ = runCLI(t, "blame", "-C", dir, "a/x.txt")
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "changed")

	code, stdout, _ = runCLI(t, "history", "-C", dir, "a/x.txt")
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "change x")

	code, stdout, _ = runCLI(t, "sparse", "-C", dir, "b")
	require.Equal(t, exitOK, code)
	require.Equal(t, "sparse: b\n", stdout)

	code, stdout, _ = runCLI(t, "update", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "up to date")
}

func TestRunAcceptsFlagsAfterPositionalArguments(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a/x.txt", []byte("main"))
	mem.Put("dev", "a/x.txt", []byte("dev"))
	mem.Put("dev", "b/y.txt", []byte("two"))
	useMemoryBackend(t, mem)

	// The syntax of the plan puts the flags after the positional arguments.
	dir := filepath.Join(t.TempDir(), "ws")
	code, stdout, stderr := runCLI(t, "clone", "memory", "repo", dir, "--branch", "dev", "--sparse", "a/")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "cloned repo (dev)")
	require.NoDirExists(t, filepath.Join(dir, "b"))

	code, stdout, stderr = runCLI(t, "blame", "a/x.txt", "-C", dir)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "dev")

	code, _, stderr = runCLI(t, "history", "a/x.txt", "-C", dir, "--rev", "main", "--limit", "5")
	require.Equal(t, exitOK, code, stderr)
}

func TestRunRejectsUnsafePathArguments(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("one"))
	useMemoryBackend(t, mem)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	for _, args := range [][]string{
		{"blame", "-C", dir, "../etc/passwd"},
		{"history", "-C", dir, "/etc/passwd"},
		{"cat", "-C", dir, "../x"},
		{"cat", "-C", dir, "main"},
	} {
		code, _, _ := runCLI(t, args...)
		require.Equal(t, exitUsage, code, strings.Join(args, " "))
	}
}

func TestRunWatchDeliversBacklog(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("one"))
	useMemoryBackend(t, mem)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, _ := runCLI(t, "watch", "-C", dir)
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "refs/heads/main")
}
