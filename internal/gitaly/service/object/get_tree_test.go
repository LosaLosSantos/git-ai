package object

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// treeEntry builds an expected entry of a GetTree listing.
func treeEntry(path string, oid git.ObjectID, typ gitalypb.ObjectType, mode int32, size int64) *gitalypb.GetTreeResponse_Entry {
	return &gitalypb.GetTreeResponse_Entry{
		Path: []byte(path),
		Oid:  oid.String(),
		Type: typ,
		Mode: mode,
		Size: size,
	}
}

// getTree sends a GetTree request and receives all responses.
func getTree(t testing.TB, ctx context.Context, client gitalypb.ObjectServiceClient, req *gitalypb.GetTreeRequest) ([]*gitalypb.GetTreeResponse, error) {
	t.Helper()

	stream, err := client.GetTree(ctx, req)
	require.NoError(t, err)

	return testhelper.Receive(stream.Recv)
}

// flattenTree concatenates the entries of all responses. The tree OID must only be set on the
// first response.
func flattenTree(t testing.TB, responses []*gitalypb.GetTreeResponse) (string, []*gitalypb.GetTreeResponse_Entry) {
	t.Helper()

	require.NotEmpty(t, responses, "at least one response is expected")

	var entries []*gitalypb.GetTreeResponse_Entry
	for i, response := range responses {
		if i == 0 {
			require.NotEmpty(t, response.GetTreeOid(), "first response must carry the tree OID")
		} else {
			require.Empty(t, response.GetTreeOid(), "tree OID is only set on the first response")
		}

		entries = append(entries, response.GetEntries()...)
	}

	return responses[0].GetTreeOid(), entries
}

// treeFixture is a repository layout with nested directories used by the GetTree tests.
type treeFixture struct {
	repo      *gitalypb.Repository
	repoPath  string
	rootTree  git.ObjectID
	commit    git.ObjectID
	blobA     git.ObjectID // "a content", 9 bytes
	blobB     git.ObjectID // "bb", 2 bytes
	dirTree   git.ObjectID
	deepTree  git.ObjectID
	submodule git.ObjectID
}

// setupTreeFixture writes the following tree, both as a bare tree and as the tree of a commit:
//
//	a.txt        blob   100644
//	bin          blob   100755
//	dir/         tree   040000
//	  deeper/    tree   040000
//	    leaf     blob   100644
//	  nested.txt blob   100644
//	sub-mod      commit 160000
func setupTreeFixture(t testing.TB, ctx context.Context, cfg config.Cfg, objectFormat string) treeFixture {
	t.Helper()

	var repo *gitalypb.Repository
	var repoPath string
	if objectFormat == "" {
		repo, repoPath = gittest.CreateRepository(t, ctx, cfg)
	} else {
		repo, repoPath = gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{ObjectFormat: objectFormat})
	}

	objectHash := gittest.DefaultObjectHash
	if objectFormat == git.ObjectHashSHA256.Format {
		objectHash = git.ObjectHashSHA256
	}

	blobA := gittest.WriteBlob(t, cfg, repoPath, []byte("a content"))
	blobB := gittest.WriteBlob(t, cfg, repoPath, []byte("bb"))
	submodule := objectHash.HashData([]byte("submodule commit"))

	deepTree := writeTree(t, cfg, repoPath, []gittest.TreeEntry{
		{OID: blobA, Mode: "100644", Path: "leaf"},
	})
	dirTree := writeTree(t, cfg, repoPath, []gittest.TreeEntry{
		{OID: deepTree, Mode: "040000", Path: "deeper"},
		{OID: blobA, Mode: "100644", Path: "nested.txt"},
	})

	entries := []gittest.TreeEntry{
		{OID: blobA, Mode: "100644", Path: "a.txt"},
		{OID: blobB, Mode: "100755", Path: "bin"},
		{OID: dirTree, Mode: "040000", Path: "dir"},
		{OID: submodule, Mode: "160000", Path: "sub-mod"},
	}

	rootTree := writeTree(t, cfg, repoPath, entries)
	commit := commitTree(t, cfg, repoPath, rootTree)

	return treeFixture{
		repo:      repo,
		repoPath:  repoPath,
		rootTree:  rootTree,
		commit:    commit,
		blobA:     blobA,
		blobB:     blobB,
		dirTree:   dirTree,
		deepTree:  deepTree,
		submodule: submodule,
	}
}

// writeTree writes a tree with git-mktree(1). Unlike gittest.WriteTree it works for every object
// format, because it does not hash object IDs with the default object hash.
func writeTree(t testing.TB, cfg config.Cfg, repoPath string, entries []gittest.TreeEntry) git.ObjectID {
	t.Helper()

	var stdin bytes.Buffer
	for _, entry := range entries {
		objectType := "blob"
		switch entry.Mode {
		case "040000":
			objectType = "tree"
		case "160000":
			objectType = "commit"
		}
		fmt.Fprintf(&stdin, "%s %s %s\t%s\n", entry.Mode, objectType, entry.OID, entry.Path)
	}

	output := gittest.ExecOpts(t, cfg, gittest.ExecConfig{Stdin: &stdin}, "-C", repoPath, "mktree")
	return git.ObjectID(strings.TrimSpace(string(output)))
}

// commitTree writes a commit for the given tree without any parents, with git-commit-tree(1).
func commitTree(t testing.TB, cfg config.Cfg, repoPath string, tree git.ObjectID) git.ObjectID {
	t.Helper()

	output := gittest.Exec(t, cfg, "-C", repoPath, "commit-tree", tree.String(), "-m", "fixture")
	return git.ObjectID(strings.TrimSpace(string(output)))
}

func TestGetTree(t *testing.T) {
	t.Parallel()

	t.Run("direct entries by tree OID", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		fixture := setupTreeFixture(t, ctx, cfg, "")

		responses, err := getTree(t, ctx, client, &gitalypb.GetTreeRequest{
			Repository: fixture.repo,
			TreeOid:    fixture.rootTree.String(),
		})
		require.NoError(t, err)

		treeOID, entries := flattenTree(t, responses)
		require.Equal(t, fixture.rootTree.String(), treeOID)
		testhelper.ProtoEqual(t, []*gitalypb.GetTreeResponse_Entry{
			treeEntry("a.txt", fixture.blobA, gitalypb.ObjectType_BLOB, 0o100644, 0),
			treeEntry("bin", fixture.blobB, gitalypb.ObjectType_BLOB, 0o100755, 0),
			treeEntry("dir", fixture.dirTree, gitalypb.ObjectType_TREE, 0o040000, 0),
			treeEntry("sub-mod", fixture.submodule, gitalypb.ObjectType_COMMIT, 0o160000, 0),
		}, entries)
	})

	t.Run("subtree by revision and path", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		fixture := setupTreeFixture(t, ctx, cfg, "")

		// Leading and trailing slashes in the path are ignored.
		responses, err := getTree(t, ctx, client, &gitalypb.GetTreeRequest{
			Repository: fixture.repo,
			Revision:   []byte(fixture.commit.String()),
			Path:       []byte("/dir/"),
		})
		require.NoError(t, err)

		treeOID, entries := flattenTree(t, responses)
		require.Equal(t, fixture.dirTree.String(), treeOID)
		testhelper.ProtoEqual(t, []*gitalypb.GetTreeResponse_Entry{
			treeEntry("deeper", fixture.deepTree, gitalypb.ObjectType_TREE, 0o040000, 0),
			treeEntry("nested.txt", fixture.blobA, gitalypb.ObjectType_BLOB, 0o100644, 0),
		}, entries)
	})

	t.Run("recursive listing is depth first with relative paths", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		fixture := setupTreeFixture(t, ctx, cfg, "")

		responses, err := getTree(t, ctx, client, &gitalypb.GetTreeRequest{
			Repository: fixture.repo,
			Revision:   []byte(fixture.commit.String()),
			Recursive:  true,
		})
		require.NoError(t, err)

		treeOID, entries := flattenTree(t, responses)
		require.Equal(t, fixture.rootTree.String(), treeOID)
		testhelper.ProtoEqual(t, []*gitalypb.GetTreeResponse_Entry{
			treeEntry("a.txt", fixture.blobA, gitalypb.ObjectType_BLOB, 0o100644, 0),
			treeEntry("bin", fixture.blobB, gitalypb.ObjectType_BLOB, 0o100755, 0),
			treeEntry("dir", fixture.dirTree, gitalypb.ObjectType_TREE, 0o040000, 0),
			treeEntry("dir/deeper", fixture.deepTree, gitalypb.ObjectType_TREE, 0o040000, 0),
			treeEntry("dir/deeper/leaf", fixture.blobA, gitalypb.ObjectType_BLOB, 0o100644, 0),
			treeEntry("dir/nested.txt", fixture.blobA, gitalypb.ObjectType_BLOB, 0o100644, 0),
			treeEntry("sub-mod", fixture.submodule, gitalypb.ObjectType_COMMIT, 0o160000, 0),
		}, entries)
	})

	t.Run("sizes are resolved for blobs only", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		fixture := setupTreeFixture(t, ctx, cfg, "")

		responses, err := getTree(t, ctx, client, &gitalypb.GetTreeRequest{
			Repository:   fixture.repo,
			Revision:     []byte(fixture.commit.String()),
			Recursive:    true,
			IncludeSizes: true,
		})
		require.NoError(t, err)

		_, entries := flattenTree(t, responses)
		testhelper.ProtoEqual(t, []*gitalypb.GetTreeResponse_Entry{
			treeEntry("a.txt", fixture.blobA, gitalypb.ObjectType_BLOB, 0o100644, 9),
			treeEntry("bin", fixture.blobB, gitalypb.ObjectType_BLOB, 0o100755, 2),
			treeEntry("dir", fixture.dirTree, gitalypb.ObjectType_TREE, 0o040000, 0),
			treeEntry("dir/deeper", fixture.deepTree, gitalypb.ObjectType_TREE, 0o040000, 0),
			treeEntry("dir/deeper/leaf", fixture.blobA, gitalypb.ObjectType_BLOB, 0o100644, 9),
			treeEntry("dir/nested.txt", fixture.blobA, gitalypb.ObjectType_BLOB, 0o100644, 9),
			treeEntry("sub-mod", fixture.submodule, gitalypb.ObjectType_COMMIT, 0o160000, 0),
		}, entries)
	})

	t.Run("sizes without recursion", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		fixture := setupTreeFixture(t, ctx, cfg, "")

		responses, err := getTree(t, ctx, client, &gitalypb.GetTreeRequest{
			Repository:   fixture.repo,
			TreeOid:      fixture.rootTree.String(),
			IncludeSizes: true,
		})
		require.NoError(t, err)

		_, entries := flattenTree(t, responses)
		testhelper.ProtoEqual(t, []*gitalypb.GetTreeResponse_Entry{
			treeEntry("a.txt", fixture.blobA, gitalypb.ObjectType_BLOB, 0o100644, gittest.ObjectSize(t, cfg, fixture.repoPath, fixture.blobA)),
			treeEntry("bin", fixture.blobB, gitalypb.ObjectType_BLOB, 0o100755, 2),
			treeEntry("dir", fixture.dirTree, gitalypb.ObjectType_TREE, 0o040000, 0),
			treeEntry("sub-mod", fixture.submodule, gitalypb.ObjectType_COMMIT, 0o160000, 0),
		}, entries)
	})

	t.Run("empty tree", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		repo, _ := gittest.CreateRepository(t, ctx, cfg)

		responses, err := getTree(t, ctx, client, &gitalypb.GetTreeRequest{
			Repository: repo,
			TreeOid:    gittest.DefaultObjectHash.EmptyTreeOID.String(),
			Recursive:  true,
		})
		require.NoError(t, err)

		// The first message always carries the tree OID, even if there are no entries.
		treeOID, entries := flattenTree(t, responses)
		require.Equal(t, gittest.DefaultObjectHash.EmptyTreeOID.String(), treeOID)
		require.Empty(t, entries)
	})

	t.Run("listing is split into batches", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		blob := gittest.WriteBlob(t, cfg, repoPath, []byte("a content"))

		const entryCount = 2005
		var entries []gittest.TreeEntry
		for i := range entryCount {
			entries = append(entries, gittest.TreeEntry{OID: blob, Mode: "100644", Path: fmt.Sprintf("file-%05d", i)})
		}
		treeOID := gittest.WriteTree(t, cfg, repoPath, entries)

		responses, err := getTree(t, ctx, client, &gitalypb.GetTreeRequest{
			Repository:   repo,
			TreeOid:      treeOID.String(),
			IncludeSizes: true,
		})
		require.NoError(t, err)

		require.Len(t, responses, 3)
		require.Len(t, responses[0].GetEntries(), 1000)
		require.Len(t, responses[1].GetEntries(), 1000)
		require.Len(t, responses[2].GetEntries(), 5)

		listed, flat := flattenTree(t, responses)
		require.Equal(t, treeOID.String(), listed)
		require.Len(t, flat, entryCount)
		for i, entry := range flat {
			require.Equal(t, fmt.Sprintf("file-%05d", i), string(entry.GetPath()))
			require.Equal(t, int64(9), entry.GetSize())
		}
	})

	t.Run("validation and lookup errors", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		fixture := setupTreeFixture(t, ctx, cfg, "")

		missingOID := gittest.DefaultObjectHash.HashData([]byte("missing tree"))

		for _, tc := range []struct {
			desc string
			req  *gitalypb.GetTreeRequest
			code codes.Code
		}{
			{
				desc: "unset repository",
				req:  &gitalypb.GetTreeRequest{TreeOid: fixture.rootTree.String()},
				code: codes.InvalidArgument,
			},
			{
				desc: "nonexistent repository",
				req: &gitalypb.GetTreeRequest{
					Repository: &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: "does-not-exist.git"},
					TreeOid:    fixture.rootTree.String(),
				},
				code: codes.NotFound,
			},
			{
				desc: "neither tree OID nor revision",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, Path: []byte("dir")},
				code: codes.InvalidArgument,
			},
			{
				desc: "tree OID and revision are mutually exclusive",
				req: &gitalypb.GetTreeRequest{
					Repository: fixture.repo,
					TreeOid:    fixture.rootTree.String(),
					Revision:   []byte(fixture.commit.String()),
				},
				code: codes.InvalidArgument,
			},
			{
				desc: "short tree OID",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, TreeOid: "abc"},
				code: codes.InvalidArgument,
			},
			{
				desc: "uppercase tree OID",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, TreeOid: strings.ToUpper(fixture.rootTree.String())},
				code: codes.InvalidArgument,
			},
			{
				desc: "revision starting with a dash",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, Revision: []byte("--output=/tmp/x")},
				code: codes.InvalidArgument,
			},
			{
				desc: "revision containing a colon",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, Revision: []byte(fixture.commit.String() + ":dir")},
				code: codes.InvalidArgument,
			},
			{
				desc: "path with parent directory component",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, Revision: []byte(fixture.commit.String()), Path: []byte("dir/../dir")},
				code: codes.InvalidArgument,
			},
			{
				desc: "path with current directory component",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, Revision: []byte(fixture.commit.String()), Path: []byte("dir/./deeper")},
				code: codes.InvalidArgument,
			},
			{
				desc: "path with empty component",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, Revision: []byte(fixture.commit.String()), Path: []byte("dir//deeper")},
				code: codes.InvalidArgument,
			},
			{
				desc: "tree OID points to a blob",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, TreeOid: fixture.blobA.String()},
				code: codes.InvalidArgument,
			},
			{
				desc: "path points to a blob",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, Revision: []byte(fixture.commit.String()), Path: []byte("a.txt")},
				code: codes.InvalidArgument,
			},
			{
				desc: "missing tree OID",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, TreeOid: missingOID.String()},
				code: codes.NotFound,
			},
			{
				desc: "missing revision",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, Revision: []byte("refs/heads/does-not-exist")},
				code: codes.NotFound,
			},
			{
				desc: "missing path",
				req:  &gitalypb.GetTreeRequest{Repository: fixture.repo, Revision: []byte(fixture.commit.String()), Path: []byte("dir/nope")},
				code: codes.NotFound,
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				_, err := getTree(t, ctx, client, tc.req)
				testhelper.RequireGrpcCode(t, err, tc.code)
			})
		}
	})

	t.Run("sha256 repository", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg, client := setup(t, ctx, nil)
		fixture := setupTreeFixture(t, ctx, cfg, git.ObjectHashSHA256.Format)

		require.Len(t, fixture.rootTree.String(), git.ObjectHashSHA256.EncodedLen())

		responses, err := getTree(t, ctx, client, &gitalypb.GetTreeRequest{
			Repository:   fixture.repo,
			TreeOid:      fixture.rootTree.String(),
			Recursive:    true,
			IncludeSizes: true,
		})
		require.NoError(t, err)

		_, entries := flattenTree(t, responses)
		require.Len(t, entries, 7)
		require.Equal(t, fixture.blobA.String(), entries[0].GetOid())
		require.Equal(t, int64(9), entries[0].GetSize())

		// A SHA1 sized tree OID is not valid in a SHA256 repository.
		_, err = getTree(t, ctx, client, &gitalypb.GetTreeRequest{
			Repository: fixture.repo,
			TreeOid:    git.ObjectHashSHA1.EmptyTreeOID.String(),
		})
		testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
	})
}
