package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
)

// reviewCommitWithFiles writes a commit on top of parent whose tree holds
// exactly files, without moving any branch.
func (e serverEnv) reviewCommitWithFiles(t *testing.T, parent git.ObjectID, files map[string]string) git.ObjectID {
	t.Helper()

	return gittest.WriteCommit(t, e.cfg, e.repoPath,
		gittest.WithTree(e.writeTree(t, files)),
		gittest.WithParents(parent),
	)
}

// TestCLIReviewAgainstServer drives the review commands against a real server.
// It runs serially because the CLI tests share the package-level backend factory.
func TestCLIReviewAgainstServer(t *testing.T) {
	env := startServer(t)
	base := env.seedMain(t, map[string]string{"x.go": "package x\n\nfunc F() {}\n"})
	feature := env.reviewCommitWithFiles(t, base, map[string]string{"x.go": "package x\n\nfunc F() int { return 1 }\n"})
	next := env.reviewCommitWithFiles(t, feature, map[string]string{"x.go": "package x\n\nfunc F() int { return 2 }\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "review", "create", "-C", dir, "--branch", "main", "--title", "fix x",
		"--description", "why", "--head", feature.String())
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "opened review ")
	require.Contains(t, stdout, "fix x (main <- ")
	id := strings.TrimSuffix(strings.Fields(stdout)[2], ":")

	code, stdout, stderr = runCLI(t, "review", "show", "-C", dir, id)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "review "+id+" (open): fix x")
	require.Contains(t, stdout, "target main")
	require.Contains(t, stdout, "why")
	require.Contains(t, stdout, "x.go")
	require.Contains(t, stdout, "approved: no")
	require.Contains(t, stdout, "code: 0 of 1 approvals (unsatisfied)")

	code, stdout, stderr = runCLI(t, "review", "show", "-C", dir, "--json", id)
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, 1, strings.Count(stdout, "\n"), "one object, one line")
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	require.Equal(t, id, got["id"])
	require.Contains(t, got, "targetBranch", "protojson names fields in lowerCamelCase")

	code, stdout, stderr = runCLI(t, "review", "list", "-C", dir, "--branch", "main")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, id+"\topen\tmain\t")

	code, stdout, stderr = runCLI(t, "review", "comment", "-C", dir, id, "--body", "nit", "--path", "x.go", "--line", "3")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "commented on review "+id+" at x.go:3")

	code, _, stderr = runCLI(t, "review", "comment", "-C", dir, id, "--body", "nit", "--line", "3")
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, "--line requires --path")

	code, stdout, stderr = runCLI(t, "review", "request-changes", "-C", dir, id, "--body", "fix it")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "requested changes on review "+id+" at ")

	code, stdout, stderr = runCLI(t, "review", "approve", "-C", dir, id)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "approved review "+id+" at ")

	code, stdout, stderr = runCLI(t, "review", "show", "-C", dir, id)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "changes requested by anonymous on")
	require.Contains(t, stdout, "anonymous x.go:3 on")
	require.Contains(t, stdout, ": nit")

	code, stdout, stderr = runCLI(t, "review", "update", "-C", dir, id, "--head", next.String())
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "review "+id+" now at ")

	code, stdout, stderr = runCLI(t, "review", "close", "-C", dir, id, "--landed")
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, "review "+id+" is landed\n", stdout)

	code, stdout, _ = runCLI(t, "review", "list", "-C", dir, "--all")
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, id+"\tlanded\tmain\t")

	code, _, stderr = runCLI(t, "review", "show", "-C", dir, "999999")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "not found")
}

// TestCLIReviewUsageErrorsDoNotContactServer checks the arguments of the review
// commands before any server call. The workspace does not exist, so a server call
// would fail with a different error.
func TestCLIReviewUsageErrorsDoNotContactServer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")

	cases := map[string][]string{
		"no subcommand":      {"review", "-C", dir},
		"unknown subcommand": {"review", "merge", "-C", dir},
		"create no branch":   {"review", "create", "-C", dir, "--title", "t"},
		"create no title":    {"review", "create", "-C", dir, "--branch", "main"},
		"create with arg":    {"review", "create", "-C", dir, "extra", "--branch", "main", "--title", "t"},
		"show no ID":         {"review", "show", "-C", dir},
		"comment no body":    {"review", "comment", "-C", dir, "1"},
		"update extra arg":   {"review", "update", "-C", dir, "1", "2"},
		"close no ID":        {"review", "close", "-C", dir},
	}
	for name, args := range cases {
		code, _, stderr := runCLI(t, args...)
		require.Equal(t, exitUsage, code, "%s: %s", name, stderr)
	}
}
