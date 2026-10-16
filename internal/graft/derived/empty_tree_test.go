package derived

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// rawTreeEntry is an entry of a tree written byte for byte by writeRawTree.
type rawTreeEntry struct {
	mode string
	name string
	oid  git.ObjectID
}

// writeRawTree writes a tree object without any validation, so that it can hold
// modes that Git only accepts when reading old repositories. The entries must be
// given in Git's tree order.
func writeRawTree(tb testing.TB, cfg config.Cfg, repoPath string, entries []rawTreeEntry) git.ObjectID {
	tb.Helper()

	var tree bytes.Buffer
	for _, entry := range entries {
		raw, err := hex.DecodeString(entry.oid.String())
		require.NoError(tb, err)

		tree.WriteString(entry.mode + " " + entry.name + "\x00")
		tree.Write(raw)
	}

	out := gittest.ExecOpts(tb, cfg, gittest.ExecConfig{Stdin: &tree},
		"-C", repoPath, "hash-object", "-t", "tree", "--literally", "-w", "--stdin")
	oid, err := gittest.DefaultObjectHash.FromHex(string(bytes.TrimSpace(out)))
	require.NoError(tb, err)
	return oid
}

// emptyTreeHistory writes a history in which paths hold empty trees, which Git's
// pathspec-limited history ignores because its diff only reports files. It
// returns the commit IDs by name.
func emptyTreeHistory(tb testing.TB, cfg config.Cfg, repoPath string) map[string]git.ObjectID {
	tb.Helper()

	tree := func(entries ...gittest.TreeEntry) git.ObjectID {
		return gittest.WriteTree(tb, cfg, repoPath, entries)
	}
	blob := func(content string) git.ObjectID {
		return gittest.WriteBlob(tb, cfg, repoPath, []byte(content))
	}

	emptyTree := tree()
	blobA := blob("a\n")
	blobX := blob("x\n")
	blobF := blob("f\n")

	nested := tree(gittest.TreeEntry{Mode: "040000", Path: "inner", OID: emptyTree})
	dirX := tree(gittest.TreeEntry{Mode: "100644", Path: "x", OID: blobX})
	dirXSub := tree(
		gittest.TreeEntry{Mode: "040000", Path: "sub", OID: emptyTree},
		gittest.TreeEntry{Mode: "100644", Path: "x", OID: blobX},
	)
	dirXOther := tree(
		gittest.TreeEntry{Mode: "040000", Path: "other", OID: emptyTree},
		gittest.TreeEntry{Mode: "100644", Path: "x", OID: blobX},
	)
	dirF := tree(gittest.TreeEntry{Mode: "100644", Path: "f", OID: blobF})

	a := gittest.TreeEntry{Mode: "100644", Path: "a", OID: blobA}
	d := func(oid git.ObjectID) gittest.TreeEntry {
		return gittest.TreeEntry{Mode: "040000", Path: "d", OID: oid}
	}
	e := func(oid git.ObjectID) gittest.TreeEntry {
		return gittest.TreeEntry{Mode: "040000", Path: "e", OID: oid}
	}
	n := gittest.TreeEntry{Mode: "040000", Path: "n", OID: nested}

	ids := make(map[string]git.ObjectID)
	commit := func(name string, treeID git.ObjectID, parents ...string) {
		parentIDs := make([]git.ObjectID, 0, len(parents))
		for _, parent := range parents {
			parentIDs = append(parentIDs, ids[parent])
		}
		ids[name] = gittest.WriteCommit(tb, cfg, repoPath,
			gittest.WithBranch(name),
			gittest.WithMessage(name),
			gittest.WithTree(treeID),
			gittest.WithParents(parentIDs...),
		)
	}

	// A root commit with the empty tree, as `git commit --allow-empty` creates it.
	commit("root", emptyTree)
	commit("c1", tree(a), "root")
	// "e" is added as an empty tree, which is not a change of "e".
	commit("c2", tree(a, d(dirX), e(emptyTree)), "c1")
	// "d" gains an empty subtree: its tree changes but it holds the same files.
	commit("c3", tree(a, d(dirXSub), e(emptyTree)), "c2")
	// "n" holds only an empty subtree.
	commit("side", tree(a, d(dirXOther), e(emptyTree), n), "c2")
	// The merge differs from its first parent only in empty trees.
	commit("merge", tree(a, d(dirXOther), e(emptyTree), n), "c3", "side")
	// Removing the empty "e" changes the root tree but no file.
	commit("c4", tree(a, d(dirXOther), n), "merge")
	// An empty tree becoming a tree with a file is a change.
	commit("c5", tree(a, d(dirXOther), e(dirF), n), "c4")
	commit("c6", tree(gittest.TreeEntry{Mode: "100755", Path: "a", OID: blobA}, d(dirXOther), e(dirF), n), "c5")
	// 100775 is read by Git as 100755, so "a" and the root are unchanged.
	commit("c7", writeRawTree(tb, cfg, repoPath, []rawTreeEntry{
		{mode: "100775", name: "a", oid: blobA},
		{mode: "40000", name: "d", oid: dirXOther},
		{mode: "40000", name: "e", oid: dirF},
		{mode: "40000", name: "n", oid: nested},
	}), "c6")

	return ids
}

var (
	emptyTreeCommits = []string{"root", "c1", "c2", "c3", "side", "merge", "c4", "c5", "c6", "c7"}
	emptyTreePaths   = []string{".", "", "a", "d", "d/x", "d/sub", "d/other", "e", "e/f", "n", "n/inner", "missing"}
)

func TestLastMod_EmptyTreesMatchGit(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := emptyTreeHistory(t, cfg, repoPath)

	// Anchors that make sure the comparison with Git below is not vacuous.
	require.Empty(t, gitLastCommitForPath(t, cfg, repoPath, ids["root"], "."))
	require.Equal(t, ids["c2"].String(), gitLastCommitForPath(t, cfg, repoPath, ids["c4"], "."))
	require.Equal(t, ids["c6"].String(), gitLastCommitForPath(t, cfg, repoPath, ids["c7"], "a"))

	for _, cache := range []bool{true, false} {
		deriver := newTestDeriver(t, ctx, cfg, repo, cache)
		for _, name := range emptyTreeCommits {
			for _, path := range emptyTreePaths {
				gitPath := path
				if gitPath == "" {
					gitPath = "."
				}
				want := gitLastCommitForPath(t, cfg, repoPath, ids[name], gitPath)

				got, err := deriver.LastMod(ctx, ids[name], path)
				require.NoError(t, err)
				require.Equal(t, want, got.String(), "lastmod of %q at %s (cache %v)", path, name, cache)
			}
		}
	}

	// A second pass over the stored values must give the same answers without walking.
	// Answers saying that the path does not exist in the history are not stored, so
	// only those are walked again.
	for _, name := range emptyTreeCommits {
		for _, path := range []string{".", "a", "d", "e", "n"} {
			want := gitLastCommitForPath(t, cfg, repoPath, ids[name], path)

			cached := newTestDeriver(t, ctx, cfg, repo, true)
			got, err := cached.LastMod(ctx, ids[name], path)
			require.NoError(t, err)
			require.Equal(t, want, got.String(), "lastmod of %q at %s", path, name)
			if want != "" {
				require.Zero(t, cached.CommitsComputed(), "lastmod of %q at %s must be stored", path, name)
			}
		}
	}
}

func TestPathHistory_EmptyTreesMatchGit(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := emptyTreeHistory(t, cfg, repoPath)

	// The empty root commit is not part of the history of the root directory.
	require.Equal(t, []string{ids["c6"].String(), ids["c5"].String(), ids["c2"].String(), ids["c1"].String()},
		gitFirstParentPathHistory(t, cfg, repoPath, ids["c7"], "."))

	for _, cache := range []bool{true, false} {
		deriver := newTestDeriver(t, ctx, cfg, repo, cache)
		for _, name := range emptyTreeCommits {
			for _, path := range emptyTreePaths {
				gitPath := path
				if gitPath == "" {
					gitPath = "."
				}
				want := gitFirstParentPathHistory(t, cfg, repoPath, ids[name], gitPath)
				got := collectPathHistory(ctx, t, deriver, ids[name], path, 1000)
				require.Equal(t, want, got, "path history of %q at %s (cache %v)", path, name, cache)
			}
		}
	}
}

func TestCanonMode(t *testing.T) {
	t.Parallel()

	for mode, want := range map[uint64]uint32{
		0o100644: 0o100644,
		0o100664: 0o100644,
		0o100600: 0o100644,
		0o100755: 0o100755,
		0o100775: 0o100755,
		0o100700: 0o100755,
		0o120000: 0o120000,
		0o040000: 0o040000,
		0o160000: 0o160000,
	} {
		require.Equal(t, want, canonMode(mode), "mode %o", mode)
	}
}
