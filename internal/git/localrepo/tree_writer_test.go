package localrepo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

func setupTreeWriterRepo(t *testing.T) (context.Context, config.Cfg, *Repo, string) {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	return ctx, cfg, NewTestRepo(t, cfg, repoProto), repoPath
}

// requireNoTreeObjectFiles asserts that no temporary tree object file is left in the storage's
// temporary directory.
func requireNoTreeObjectFiles(t *testing.T, repo *Repo) {
	t.Helper()

	tempDir, err := repo.StorageTempDir()
	require.NoError(t, err)
	entries, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), "tree-object-")
	}
}

func cloneEntries(entries []*TreeEntry) []*TreeEntry {
	cloned := make([]*TreeEntry, 0, len(entries))
	for _, entry := range entries {
		entryCopy := *entry
		cloned = append(cloned, &entryCopy)
	}
	return cloned
}

// TestTreeWriter_matchesWriteEntries checks that the tree writer writes the trees today's writer
// writes, with the same IDs, through one process for every tree, and refuses the trees it refuses,
// with the same error (PLAN9, WP96).
func TestTreeWriter_matchesWriteEntries(t *testing.T) {
	t.Parallel()

	ctx, cfg, repo, repoPath := setupTreeWriterRepo(t)

	blob := gittest.WriteBlob(t, cfg, repoPath, []byte("one\n"))
	otherBlob := gittest.WriteBlob(t, cfg, repoPath, []byte("two\n"))
	subtree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{{Path: "x", Mode: "100644", OID: blob}})
	commit := gittest.WriteCommit(t, cfg, repoPath)
	// An object ID that no object of the repository has.
	missing := gittest.DefaultObjectHash.HashData([]byte("missing object"))

	for _, tc := range []struct {
		desc    string
		entries []*TreeEntry
		refused bool
	}{
		{
			desc: "empty",
		},
		{
			desc: "edge names",
			entries: []*TreeEntry{
				{Path: "a", Mode: "100644", Type: Blob, OID: blob},
				{Path: "a.b", Mode: "100644", Type: Blob, OID: otherBlob},
				{Path: "a b", Mode: "100644", Type: Blob, OID: blob},
				{Path: "é té", Mode: "100644", Type: Blob, OID: blob},
				{Path: "-", Mode: "100644", Type: Blob, OID: blob},
			},
		},
		{
			desc: "a directory sorts as its name with a slash",
			entries: []*TreeEntry{
				{Path: "a0", Mode: "100644", Type: Blob, OID: blob},
				{Path: "a", Mode: "040000", Type: Tree, OID: subtree},
				{Path: "a.b", Mode: "100644", Type: Blob, OID: blob},
				{Path: "a-", Mode: "100644", Type: Blob, OID: blob},
			},
		},
		{
			desc: "every mode",
			entries: []*TreeEntry{
				{Path: "regular", Mode: "100644", Type: Blob, OID: blob},
				{Path: "executable", Mode: "100755", Type: Blob, OID: blob},
				{Path: "symlink", Mode: "120000", Type: Blob, OID: otherBlob},
				{Path: "directory", Mode: "040000", Type: Tree, OID: subtree},
				{Path: "submodule", Mode: "160000", Type: Submodule, OID: commit},
			},
		},
		{
			desc: "an object that does not exist is accepted",
			entries: []*TreeEntry{
				{Path: "missing", Mode: "100644", Type: Blob, OID: missing},
				{Path: "missing-submodule", Mode: "160000", Type: Submodule, OID: missing},
			},
		},
		{
			desc: "duplicate names",
			entries: []*TreeEntry{
				{Path: "d", Mode: "100644", Type: Blob, OID: blob},
				{Path: "d", Mode: "100644", Type: Blob, OID: otherBlob},
			},
			refused: true,
		},
		{
			desc: "a file and a directory of the same name",
			entries: []*TreeEntry{
				{Path: "d", Mode: "100644", Type: Blob, OID: blob},
				{Path: "d", Mode: "040000", Type: Tree, OID: subtree},
			},
			refused: true,
		},
		{
			desc:    "invalid mode",
			entries: []*TreeEntry{{Path: "bad", Mode: "100600", Type: Blob, OID: blob}},
			refused: true,
		},
		{
			desc:    "an entry without object ID",
			entries: []*TreeEntry{{Path: "empty", Mode: "100644", Type: Blob}},
			refused: true,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			expectedOID, expectedErr := repo.writeEntries(ctx, cloneEntries(tc.entries))
			require.Equal(t, tc.refused, expectedErr != nil, "today's writer: %v", expectedErr)

			writer, err := repo.NewTreeWriter(ctx)
			require.NoError(t, err)

			oid, err := writer.write(cloneEntries(tc.entries))
			if tc.refused {
				require.Error(t, err)
				require.Equal(t, expectedErr.Error(), err.Error())

				// The process exited and was reaped; the writer refuses every later write.
				require.Nil(t, writer.cmd)
				_, laterErr := writer.write(cloneEntries(tc.entries[:1]))
				require.Equal(t, err, laterErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, expectedOID, oid)

				// The same process writes the next tree.
				process := writer.cmd
				oid, err = writer.write(cloneEntries(tc.entries))
				require.NoError(t, err)
				require.Equal(t, expectedOID, oid)
				require.Same(t, process, writer.cmd)
			}

			require.NoError(t, writer.Close())
			require.Nil(t, writer.cmd)
			require.NoError(t, writer.Close())
			_, err = writer.write(cloneEntries(tc.entries))
			require.Error(t, err)

			requireNoTreeObjectFiles(t, repo)
		})
	}
}

// TestTreeWriter_panic checks that a panic during a write, while the tree object file exists and
// the process runs, removes the file, and that Close, as a deferred call runs it on the panic's
// way out, ends the process.
func TestTreeWriter_panic(t *testing.T) {
	t.Parallel()

	ctx, cfg, repo, repoPath := setupTreeWriterRepo(t)
	blob := gittest.WriteBlob(t, cfg, repoPath, []byte("content\n"))

	writer, err := repo.NewTreeWriter(ctx)
	require.NoError(t, err)
	var fileSeen bool
	writer.SetTestHook(func(point string) {
		require.Equal(t, "started", point)
		tempDir, err := repo.StorageTempDir()
		require.NoError(t, err)
		entries, err := os.ReadDir(tempDir)
		require.NoError(t, err)
		for _, entry := range entries {
			fileSeen = fileSeen || strings.HasPrefix(entry.Name(), "tree-object-")
		}
		require.NotNil(t, writer.cmd)
		panic("injected")
	})

	require.PanicsWithValue(t, "injected", func() {
		defer func() { require.NoError(t, writer.Close()) }()
		_, _ = writer.write([]*TreeEntry{{Path: "file", Mode: "100644", Type: Blob, OID: blob}})
	})

	require.True(t, fileSeen, "the hook ran while the tree object file existed")
	require.Nil(t, writer.cmd)
	requireNoTreeObjectFiles(t, repo)
}

// TestTreeEntry_WriteWith checks that WriteWith writes the trees Write writes.
func TestTreeEntry_WriteWith(t *testing.T) {
	t.Parallel()

	ctx, cfg, repo, repoPath := setupTreeWriterRepo(t)
	blob := gittest.WriteBlob(t, cfg, repoPath, []byte("content\n"))

	build := func() *TreeEntry {
		root := &TreeEntry{Mode: "040000", Type: Tree}
		for _, path := range []string{"a/b/c", "a/b.c", "a.b/c", "d", "a/é/f"} {
			require.NoError(t, root.Add(path, TreeEntry{Path: path[strings.LastIndex(path, "/")+1:], Mode: "100644", Type: Blob, OID: blob}))
		}
		return root
	}

	expected := build()
	require.NoError(t, expected.Write(ctx, repo))

	actual := build()
	writer, err := repo.NewTreeWriter(ctx)
	require.NoError(t, err)
	require.NoError(t, actual.WriteWith(writer))
	require.NoError(t, writer.Close())

	require.Equal(t, expected, actual)
	requireNoTreeObjectFiles(t, repo)
}

// TestReadTreeLazily_matchesReadTree checks that the lazy tree, once walked, is the tree ReadTree
// reads whole, and that it refuses what ReadTree refuses.
func TestReadTreeLazily_matchesReadTree(t *testing.T) {
	t.Parallel()

	ctx, cfg, repo, repoPath := setupTreeWriterRepo(t)

	blob := gittest.WriteBlob(t, cfg, repoPath, []byte("content\n"))
	commit := gittest.WriteCommit(t, cfg, repoPath)
	inner := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
		{Path: "file", Mode: "100644", OID: blob},
		{Path: "é", Mode: "100755", OID: blob},
	})
	middle := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
		{Path: "inner", Mode: "040000", OID: inner},
		{Path: "link", Mode: "120000", OID: blob},
	})
	root := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{
		{Path: "a", Mode: "040000", OID: middle},
		{Path: "a.b", Mode: "100644", OID: blob},
		{Path: "a b", Mode: "040000", OID: inner},
		{Path: "submodule", Mode: "160000", OID: commit},
	})

	expected, err := repo.ReadTree(ctx, root.Revision(), WithRecursive())
	require.NoError(t, err)

	reader, cancel, err := repo.RequestObjectReader(ctx)
	require.NoError(t, err)
	defer cancel()

	lazy, err := repo.ReadTreeLazily(ctx, reader, root)
	require.NoError(t, err)
	// Only the root is read.
	for _, entry := range lazy.Entries {
		require.Nil(t, entry.Entries)
	}

	require.NoError(t, lazy.Walk(func(string, *TreeEntry) error { return nil }))
	require.Equal(t, expected, lazy)

	_, err = repo.ReadTreeLazily(ctx, reader, blob)
	require.Equal(t, ErrNotTreeish, err)

	missing := gittest.DefaultObjectHash.HashData([]byte("missing tree"))
	_, err = repo.ReadTreeLazily(ctx, reader, missing)
	require.ErrorContains(t, err, "check object type: object not found")
}

// TestReadTreeLazily_canonicalModes checks that the lazy tree reads legacy modes, which git no
// longer writes but still reads, as ls-tree prints them: canonical, as git's canon_mode makes
// them. Written back, they are then modes the tree writer accepts.
func TestReadTreeLazily_canonicalModes(t *testing.T) {
	t.Parallel()

	ctx, cfg, repo, repoPath := setupTreeWriterRepo(t)

	blob := gittest.WriteBlob(t, cfg, repoPath, []byte("content\n"))
	subtree := gittest.WriteTree(t, cfg, repoPath, []gittest.TreeEntry{{Path: "f", Mode: "100644", OID: blob}})
	raw := func(oid git.ObjectID) []byte {
		b, err := oid.Bytes()
		require.NoError(t, err)
		return b
	}
	var content bytes.Buffer
	for _, entry := range []struct {
		mode, name string
		oid        git.ObjectID
	}{
		// In git's order: a directory sorts as its name with a slash.
		{"100600", "a", blob},
		{"100664", "b", blob},
		{"100775", "c", blob},
		{"120777", "d", blob},
		{"40755", "e", subtree},
		{"644", "f", blob},
	} {
		fmt.Fprintf(&content, "%s %s\x00", entry.mode, entry.name)
		content.Write(raw(entry.oid))
	}
	root := git.ObjectID(text.ChompBytes(gittest.ExecOpts(t, cfg, gittest.ExecConfig{Stdin: &content},
		"-C", repoPath, "hash-object", "-t", "tree", "-w", "--literally", "--stdin")))

	expected, err := repo.ReadTree(ctx, root.Revision(), WithRecursive())
	require.NoError(t, err)

	reader, cancel, err := repo.RequestObjectReader(ctx)
	require.NoError(t, err)
	defer cancel()
	lazy, err := repo.ReadTreeLazily(ctx, reader, root)
	require.NoError(t, err)
	require.NoError(t, lazy.Walk(func(string, *TreeEntry) error { return nil }))
	require.Equal(t, expected, lazy)

	typeNames := map[ObjectType]string{Blob: "blob", Tree: "tree", Submodule: "commit"}
	var modes []string
	for _, entry := range lazy.Entries {
		modes = append(modes, entry.Mode+" "+typeNames[entry.Type]+" "+entry.Path)
	}
	require.Equal(t, []string{
		"100644 blob a", "100644 blob b", "100755 blob c", "120000 blob d", "040000 tree e", "160000 commit f",
	}, modes)

	// Written back, the tree holds the canonical modes, which the tree writer accepts.
	lazy.OID = ""
	writer, err := repo.NewTreeWriter(ctx)
	require.NoError(t, err)
	require.NoError(t, lazy.WriteWith(writer))
	require.NoError(t, writer.Close())
	require.NotEqual(t, root, lazy.OID)
}

// TestTreeWriter_quarantine checks that the request's reader and tree writer run on the
// quarantined repository: they write into the quarantine and read what only it holds.
func TestTreeWriter_quarantine(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := NewTestRepo(t, cfg, repoProto)

	quarantineDir, cleanup, err := quarantine.New(ctx, repoProto, testhelper.NewLogger(t), config.NewLocator(cfg))
	require.NoError(t, err)
	t.Cleanup(cleanup)
	quarantineRepo := NewTestRepo(t, cfg, quarantineDir.QuarantinedRepo())

	blob, err := quarantineRepo.WriteBlob(ctx, strings.NewReader("quarantined\n"), WriteBlobConfig{})
	require.NoError(t, err)

	tree := &TreeEntry{Mode: "040000", Type: Tree}
	require.NoError(t, tree.Add("dir/file", TreeEntry{Path: "file", Mode: "100644", Type: Blob, OID: blob}))
	writer, err := quarantineRepo.NewTreeWriter(ctx)
	require.NoError(t, err)
	require.NoError(t, tree.WriteWith(writer))
	require.NoError(t, writer.Close())

	// The trees are in the quarantine only.
	for _, oid := range []git.ObjectID{tree.OID, tree.Entries[0].OID} {
		_, err := quarantineRepo.ReadObject(ctx, oid)
		require.NoError(t, err)
		_, err = repo.ReadObject(ctx, oid)
		require.Equal(t, InvalidObjectError(oid.String()), err)
	}

	reader, cancel, err := quarantineRepo.RequestObjectReader(ctx)
	require.NoError(t, err)
	defer cancel()

	resolved, err := quarantineRepo.ResolveRevisionWithReader(ctx, reader, tree.OID.Revision()+"^{tree}")
	require.NoError(t, err)
	require.Equal(t, tree.OID, resolved)

	lazy, err := quarantineRepo.ReadTreeLazily(ctx, reader, tree.OID)
	require.NoError(t, err)
	entry, err := lazy.Get("dir/file")
	require.NoError(t, err)
	require.Equal(t, blob, entry.OID)

	mainReader, cancelMain, err := repo.RequestObjectReader(ctx)
	require.NoError(t, err)
	defer cancelMain()

	_, err = repo.ResolveRevisionWithReader(ctx, mainReader, tree.OID.Revision()+"^{tree}")
	require.Equal(t, git.ErrReferenceNotFound, err)
}
