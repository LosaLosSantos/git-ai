package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// reviewHeaderFields returns the fields of the "target ..." header line of a
// review shown for humans: target, branch, base, base ID, head, head ID, ...
func reviewHeaderFields(t *testing.T, shown string) []string {
	t.Helper()

	for _, line := range strings.Split(shown, "\n") {
		if strings.HasPrefix(line, "target ") {
			return strings.Fields(line)
		}
	}
	require.FailNow(t, "no target line in review output", shown)
	return nil
}

// TestCLIReviewCreateDefaultsToPendingCommit checks that review create without
// --head reviews the pending commit of the workspace, the change that graft
// commit made and did not land. Before the change it reviewed the base commit,
// which gave an empty review that the server reported as approved.
func TestCLIReviewCreateDefaultsToPendingCommit(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"x.go": "package x\n\nfunc F() {}\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, _, stderr = runCLI(t, "review", "create", "-C", dir, "--branch", "main", "--title", "t")
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, "no pending commit")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n\nfunc F() int { return 1 }\n"), 0o644))
	code, _, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "change x")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "review", "create", "-C", dir, "--branch", "main", "--title", "fix x")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "opened review ")
	id := strings.TrimSuffix(strings.Fields(stdout)[2], ":")

	code, stdout, stderr = runCLI(t, "review", "show", "-C", dir, id)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "x.go", "the pending change is listed as a file of the review")
	require.Contains(t, stdout, "approved: no")

	fields := reviewHeaderFields(t, stdout)
	require.GreaterOrEqual(t, len(fields), 6, "target, base and head fields: %q", fields)
	require.NotEqual(t, fields[3], fields[5], "the head is the pending commit, not the base")
}

// TestCLIReviewUpdateDefaultsToPendingCommit checks that review update without
// --head moves the review to the pending commit of the workspace, and that it
// is refused when nothing is pending.
func TestCLIReviewUpdateDefaultsToPendingCommit(t *testing.T) {
	env := startServer(t)
	base := env.seedMain(t, map[string]string{"x.go": "package x\n\nfunc F() {}\n"})
	feature := env.reviewCommitWithFiles(t, base, map[string]string{"x.go": "package x\n\nfunc F() int { return 1 }\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "review", "create", "-C", dir, "--branch", "main", "--title", "fix x", "--head", feature.String())
	require.Equal(t, exitOK, code, stderr)
	id := strings.TrimSuffix(strings.Fields(stdout)[2], ":")

	code, _, stderr = runCLI(t, "review", "update", "-C", dir, id)
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, "no pending commit")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n\nfunc F() int { return 2 }\n"), 0o644))
	code, _, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "change x")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr = runCLI(t, "review", "update", "-C", dir, id)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "review "+id+" now at ")

	code, stdout, stderr = runCLI(t, "review", "show", "-C", dir, id)
	require.Equal(t, exitOK, code, stderr)
	fields := reviewHeaderFields(t, stdout)
	require.GreaterOrEqual(t, len(fields), 6, "target, base and head fields: %q", fields)
	require.NotEqual(t, shortOID(feature.String()), fields[5], "the review moved to the pending commit")
}

// TestCLIReviewVerdictHeadIsRefusedWhenStale checks that approve and
// request-changes bind the verdict to the --head the reviewer saw. After the
// author moved the review to another head, the stale verdict fails and the
// verdict on the current head goes through.
func TestCLIReviewVerdictHeadIsRefusedWhenStale(t *testing.T) {
	env := startServer(t)
	base := env.seedMain(t, map[string]string{"x.go": "package x\n\nfunc F() {}\n"})
	reviewed := env.reviewCommitWithFiles(t, base, map[string]string{"x.go": "package x\n\nfunc F() int { return 1 }\n"})
	moved := env.reviewCommitWithFiles(t, reviewed, map[string]string{"x.go": "package x\n\nfunc F() int { return 99 }\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "review", "create", "-C", dir, "--branch", "main", "--title", "fix x", "--head", reviewed.String())
	require.Equal(t, exitOK, code, stderr)
	id := strings.TrimSuffix(strings.Fields(stdout)[2], ":")

	code, _, stderr = runCLI(t, "review", "update", "-C", dir, id, "--head", moved.String())
	require.Equal(t, exitOK, code, stderr)

	code, _, stderr = runCLI(t, "review", "approve", "-C", dir, id, "--head", reviewed.String())
	require.NotEqual(t, exitOK, code, "an approval of a head the review has left must fail")
	require.Contains(t, stderr, "review head is")

	code, _, stderr = runCLI(t, "review", "request-changes", "-C", dir, id, "--head", reviewed.String(), "--body", "wait")
	require.NotEqual(t, exitOK, code, stderr)
	require.Contains(t, stderr, "review head is")

	code, stdout, stderr = runCLI(t, "review", "approve", "-C", dir, id, "--head", moved.String())
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "approved review "+id+" at "+shortOID(moved.String()))

	code, stdout, stderr = runCLI(t, "review", "request-changes", "-C", dir, id, "--body", "wait")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "requested changes on review "+id+" at "+shortOID(moved.String()), "without --head the current head is used")
}

// TestCLIReviewWithoutWorkspaceUsesFlags checks that the review commands work
// outside a workspace when -address, -storage and -repo name the server and
// --head names the commit.
func TestCLIReviewWithoutWorkspaceUsesFlags(t *testing.T) {
	env := startServer(t)
	base := env.seedMain(t, map[string]string{"x.go": "package x\n\nfunc F() {}\n"})
	feature := env.reviewCommitWithFiles(t, base, map[string]string{"x.go": "package x\n\nfunc F() int { return 1 }\n"})

	missing := filepath.Join(t.TempDir(), "no-workspace")
	code, stdout, stderr := runCLI(t, "review", "create", "-C", missing, "-address", env.addr, "-storage", env.storage,
		"-repo", env.relPath, "--branch", "main", "--title", "remote only", "--head", feature.String())
	require.Equal(t, exitOK, code, stderr)
	id := strings.TrimSuffix(strings.Fields(stdout)[2], ":")

	code, stdout, stderr = runCLI(t, "review", "show", "-C", missing, "-address", env.addr, "-storage", env.storage,
		"-repo", env.relPath, id)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "review "+id+" (open): remote only")

	code, _, stderr = runCLI(t, "review", "create", "-C", missing, "-address", env.addr, "-storage", env.storage,
		"-repo", env.relPath, "--branch", "main", "--title", "no head")
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, "needs --head")
}

// TestCLIReviewRejectsLineOutOfRange checks that --line is refused before the
// server is contacted when it does not fit the 32-bit line number of the server.
func TestCLIReviewRejectsLineOutOfRange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")

	for _, line := range []string{"4294967297", "2147483648", "-1"} {
		code, _, stderr := runCLI(t, "review", "comment", "-C", dir, "1", "--body", "x", "--path", "x.go", "--line", line)
		require.Equal(t, exitUsage, code, "line %s: %s", line, stderr)
		require.Contains(t, stderr, "--line must be between 0 and 2147483647")
	}
}
