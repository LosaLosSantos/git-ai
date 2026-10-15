package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCLIReviewShowEscapesServerText checks that graft review show prints the
// text of other clients and the file names as displayText shows them. A raw
// escape in a title, a comment or a file name would move the cursor of the
// terminal and overwrite the "approved: no" line.
func TestCLIReviewShowEscapesServerText(t *testing.T) {
	env := startServer(t)
	base := env.seedMain(t, map[string]string{"x.go": "package x\n\nfunc F() {}\n"})
	feature := env.reviewCommitWithFiles(t, base, map[string]string{
		"x.go":            "package x\n\nfunc F() int { return 1 }\n",
		"y\x1b[2Kz.go":    "y\n",
		"multi\nline.txt": "m\n",
	})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "review", "create", "-C", dir, "--branch", "main",
		"--title", "fix\x1b[1A\x1b[2Kforged", "--description", "first line\nsecond\x1b[2K", "--head", feature.String())
	require.Equal(t, exitOK, code, stderr)
	id := strings.TrimSuffix(strings.Fields(stdout)[2], ":")

	code, _, stderr = runCLI(t, "review", "comment", "-C", dir, id, "--body", "nit\x1b[3A\x1b[2Kapproved: yes")
	require.Equal(t, exitOK, code, stderr)

	code, _, stderr = runCLI(t, "review", "request-changes", "-C", dir, id, "--body", "wait\x1b[2K\ntwo")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr = runCLI(t, "review", "show", "-C", dir, id)
	require.Equal(t, exitOK, code, stderr)
	require.NotContains(t, stdout, "\x1b", "no raw escape reaches the terminal")
	require.NotContains(t, stdout, "\r")

	require.Contains(t, stdout, "review "+id+" (open): "+`fix\x1b[1A\x1b[2Kforged`+"\n")
	require.Contains(t, stdout, "\nfirst line\nsecond"+`\x1b[2K`+"\n", "a description keeps its newlines and escapes each line")
	require.Contains(t, stdout, `anonymous on `)
	require.Contains(t, stdout, `: nit\x1b[3A\x1b[2Kapproved: yes`+"\n", "the comment stays on one line")
	require.Contains(t, stdout, "\napproved: no\n", "the real approval line is intact")
	require.Contains(t, stdout, "changes requested by anonymous on")
	require.Contains(t, stdout, `: wait\x1b[2K`+"\n    two\n", "a continuation line of a body is indented")
	require.Contains(t, stdout, `y\x1b[2Kz.go`)
	require.Contains(t, stdout, `multi\x0aline.txt`, "a newline in a file name does not break the files list")
}

// TestCLIReviewListEscapesServerText checks that a title with a tab and a newline
// cannot forge a row of graft review list.
func TestCLIReviewListEscapesServerText(t *testing.T) {
	env := startServer(t)
	base := env.seedMain(t, map[string]string{"x.go": "package x\n\nfunc F() {}\n"})
	feature := env.reviewCommitWithFiles(t, base, map[string]string{"x.go": "package x\n\nfunc F() int { return 1 }\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, _, stderr = runCLI(t, "review", "create", "-C", dir, "--branch", "main",
		"--title", "t\n42\tlanded\tmain\tdeadbeef\tforged", "--head", feature.String())
	require.Equal(t, exitOK, code, stderr)
	code, _, stderr = runCLI(t, "review", "create", "-C", dir, "--branch", "main", "--title", "second", "--head", feature.String())
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "review", "list", "-C", dir, "--branch", "main")
	require.Equal(t, exitOK, code, stderr)

	rows := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	require.Len(t, rows, 2, "one row per review: %q", stdout)
	for _, row := range rows {
		require.Equal(t, 4, strings.Count(row, "\t"), "five columns per row: %q", row)
	}
	require.NotContains(t, stdout, "\tlanded\tmain\tdeadbeef\t", "no fake landed review")
	require.Contains(t, stdout, `t\x0a42\x09landed\x09main\x09deadbeef\x09forged`)
}

// TestCLIReviewRejectsRepoInsideWorkspace checks that -repo cannot silently name
// another repository than the one of the workspace.
func TestCLIReviewRejectsRepoInsideWorkspace(t *testing.T) {
	env := startServer(t)
	base := env.seedMain(t, map[string]string{"x.go": "package x\n\nfunc F() {}\n"})
	feature := env.reviewCommitWithFiles(t, base, map[string]string{"x.go": "package x\n\nfunc F() int { return 1 }\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "review", "create", "-C", dir, "--branch", "main", "--title", "t", "--head", feature.String())
	require.Equal(t, exitOK, code, stderr)
	id := strings.TrimSuffix(strings.Fields(stdout)[2], ":")

	code, stdout, stderr = runCLI(t, "review", "show", "-C", dir, "-repo", "does/not/exist.git", id)
	require.Equal(t, exitUsage, code, stdout)
	require.Contains(t, stderr, `"does/not/exist.git"`)
	require.NotContains(t, stdout, "review "+id)

	code, stdout, stderr = runCLI(t, "review", "show", "-C", dir, "-repo", env.relPath, id)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "review "+id+" (open): t")
}

// TestCLIReviewVerdictAcceptsShortHead checks that --head may be the prefix of
// the head that show prints. A prefix of the current head is resolved to the full
// ID, and a prefix of a head the review has left is refused.
func TestCLIReviewVerdictAcceptsShortHead(t *testing.T) {
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

	code, _, stderr = runCLI(t, "review", "approve", "-C", dir, id, "--head", reviewed.String()[:12])
	require.NotEqual(t, exitOK, code, "a prefix of a head the review has left must fail")
	require.Contains(t, stderr, "review head is")

	code, stdout, stderr = runCLI(t, "review", "approve", "-C", dir, id, "--head", moved.String()[:12])
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "approved review "+id+" at "+shortOID(moved.String()))

	code, stdout, stderr = runCLI(t, "review", "show", "-C", dir, id)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "approved by anonymous on "+shortOID(moved.String()))
}

// TestDisplayLinesIndentsContinuationLines checks the layout of multi-line text:
// the newlines stay, each line is escaped, and the lines after the first get the
// indent.
func TestDisplayLinesIndentsContinuationLines(t *testing.T) {
	require.Equal(t, `a\x1b`+"\n    b", displayLines("a\x1b\nb", "    "))
	require.Equal(t, "one\ntwo", displayLines("one\ntwo", ""))
	require.Equal(t, "single", displayLines("single", "    "))
}
