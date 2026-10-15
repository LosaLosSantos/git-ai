package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/graft/workspace"
	"github.com/stretchr/testify/require"
)

// detailBackend is a memory backend that also describes commits.
type detailBackend struct {
	*workspace.MemoryBackend
	details commitDetails
}

func (b detailBackend) CommitDetails(context.Context, string) (commitDetails, error) {
	return b.details, nil
}

func TestShowJSONIsOneObjectOfTheSuperfanFormat(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("one"))
	want := commitDetails{
		OID:      strings.Repeat("c", 40),
		Parents:  []string{strings.Repeat("b", 40)},
		Author:   "Ada <ada@example.com>",
		Subject:  `Revert "x"`,
		Message:  "Revert \"x\"\n\nGraft-Auto-Revert: abc",
		Trailers: map[string][]string{"Graft-Auto-Revert": {"abc"}},
		Files:    []fileDetails{{Path: "ledger/a.go", Additions: 2, Deletions: 1}},
	}
	previous := backendFactory
	backendFactory = func(context.Context, target) (workspace.Backend, error) {
		return detailBackend{MemoryBackend: mem, details: want}, nil
	}
	t.Cleanup(func() { backendFactory = previous })

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "show", "-C", dir, "--json", "main")
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, 1, strings.Count(stdout, "\n"), "one object, one line")

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	for _, key := range []string{"oid", "parents", "author", "subject", "message", "trailers", "files"} {
		require.Contains(t, got, key)
	}
	var back commitDetails
	require.NoError(t, json.Unmarshal([]byte(stdout), &back))
	require.Equal(t, want, back)
}

func TestShowJSONListsAreNeverNull(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("one"))
	previous := backendFactory
	backendFactory = func(context.Context, target) (workspace.Backend, error) {
		return detailBackend{MemoryBackend: mem, details: commitDetails{OID: strings.Repeat("d", 40)}}, nil
	}
	t.Cleanup(func() { backendFactory = previous })

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "show", "-C", dir, "--json", "main")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, `"parents":[]`)
	require.Contains(t, stdout, `"trailers":{}`)
	require.Contains(t, stdout, `"files":[]`)
}

func TestShowNeedsOneRevision(t *testing.T) {
	code, _, stderr := runCLI(t, "show", "-C", t.TempDir())
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "show requires REV")
}

func TestShowAgainstServer(t *testing.T) {
	env := startServer(t)
	root := env.seedMain(t, map[string]string{"a/x.txt": "ax\n", "top.txt": "top\n"})
	child := gittest.WriteCommit(t, env.cfg, env.repoPath,
		gittest.WithParents(root),
		gittest.WithTree(env.writeTree(t, map[string]string{
			"a/x.txt":     "changed\n",
			"top.txt":     "top\n",
			"ledger/y.go": "one\ntwo\n",
		})),
		gittest.WithBranch("main"),
		gittest.WithMessage("Revert \"x\"\n\nBody of the revert.\n\nGraft-Auto-Revert: abc123\n"),
	)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "show", "-C", dir, "--json", child.String())
	require.Equal(t, exitOK, code, stderr)
	var got commitDetails
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	require.Equal(t, child.String(), got.OID)
	require.Equal(t, []string{root.String()}, got.Parents)
	require.Equal(t, `Revert "x"`, got.Subject)
	require.Contains(t, got.Message, "Body of the revert.")
	require.Contains(t, got.Author, "<", "name and email")
	require.Equal(t, map[string][]string{"Graft-Auto-Revert": {"abc123"}}, got.Trailers)
	require.ElementsMatch(t, []fileDetails{
		{Path: "a/x.txt", Additions: 1, Deletions: 1},
		{Path: "ledger/y.go", Additions: 2, Deletions: 0},
	}, got.Files)

	// A root commit is compared with the empty tree: every file is an addition.
	code, stdout, stderr = runCLI(t, "show", "-C", dir, "--json", root.String())
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, `"parents":[]`)
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	require.ElementsMatch(t, []fileDetails{
		{Path: "a/x.txt", Additions: 1, Deletions: 0},
		{Path: "top.txt", Additions: 1, Deletions: 0},
	}, got.Files)

	code, stdout, _ = runCLI(t, "show", "-C", dir, "main")
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "commit "+child.String())
	require.Contains(t, stdout, "+2 -0\tledger/y.go")
}

func TestShowListsTheOldPathOfARename(t *testing.T) {
	env := startServer(t)
	root := env.seedMain(t, map[string]string{"ledger/secret.go": "one\ntwo\nthree\nfour\nfive\n"})
	child := gittest.WriteCommit(t, env.cfg, env.repoPath,
		gittest.WithParents(root),
		gittest.WithTree(env.writeTree(t, map[string]string{
			"tmp/secret.go": "one\ntwo\nthree\nfour\nfive\n",
		})),
		gittest.WithBranch("main"),
		gittest.WithMessage("move it\n\nBody here.\n"),
	)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "show", "-C", dir, "--json", child.String())
	require.Equal(t, exitOK, code, stderr)
	var got commitDetails
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))

	// The protected path must appear, so that protected-path rules see the move.
	require.ElementsMatch(t, []fileDetails{
		{Path: "tmp/secret.go", Status: "renamed"},
		{Path: "ledger/secret.go", Status: "renamed-from"},
	}, got.Files)
	require.Equal(t, "move it\n\nBody here.\n", got.Message, "the message is not repeated")
}

// A tag push records the tag's ID as new_oid. The details of the tag are those
// of the tagged commit, and say which object was peeled, so that graft-superfan
// can match them with the event.
func TestShowPeelsAnAnnotatedTag(t *testing.T) {
	env := startServer(t)
	root := env.seedMain(t, map[string]string{"ledger/a.go": "a\n"})
	tag := gittest.WriteTag(t, env.cfg, env.repoPath, "v1.0", root.Revision(), gittest.WriteTagConfig{Message: "release\n"})
	require.NotEqual(t, root, tag, "an annotated tag is an object of its own")

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "show", "-C", dir, "--json", tag.String())
	require.Equal(t, exitOK, code, stderr)
	var got commitDetails
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	require.Equal(t, root.String(), got.OID)
	require.Equal(t, tag.String(), got.PeeledFrom)
	require.Equal(t, []fileDetails{{Path: "ledger/a.go", Additions: 1}}, got.Files)

	// A commit ID is not peeled: no peeled_from.
	code, stdout, stderr = runCLI(t, "show", "-C", dir, "--json", root.String())
	require.Equal(t, exitOK, code, stderr)
	require.NotContains(t, stdout, "peeled_from")
}

// Paths with characters that git allows, a backslash and a tab, are printed as
// they are: graft-superfan matches them and must see every file of the commit.
func TestShowJSONKeepsOddPaths(t *testing.T) {
	env := startServer(t)
	commit := env.seedMain(t, map[string]string{
		"ledger/keys.go": "k\n",
		`docs/a\b.md`:    "a\n",
		"docs/c\td.md":   "c\n",
	})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "show", "-C", dir, "--json", commit.String())
	require.Equal(t, exitOK, code, stderr)
	var got commitDetails
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	require.ElementsMatch(t, []fileDetails{
		{Path: "ledger/keys.go", Additions: 1},
		{Path: `docs/a\b.md`, Additions: 1},
		{Path: "docs/c\td.md", Additions: 1},
	}, got.Files)
}

func TestIsFullObjectID(t *testing.T) {
	require.True(t, isFullObjectID(strings.Repeat("a", 40)))
	require.True(t, isFullObjectID(strings.Repeat("F", 64)))
	require.False(t, isFullObjectID(strings.Repeat("a", 39)))
	require.False(t, isFullObjectID(strings.Repeat("g", 40)))
	require.False(t, isFullObjectID("main"))
}

// A root commit of a SHA-256 repository is diffed against the SHA-256 empty tree.
func TestEmptyTreeFollowsTheObjectFormat(t *testing.T) {
	require.Equal(t, "4b825dc642cb6eb9a060e54bf8d69288fbee4904", emptyTreeFor(strings.Repeat("a", 40)))
	require.Equal(t, "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321", emptyTreeFor(strings.Repeat("a", 64)))
}
