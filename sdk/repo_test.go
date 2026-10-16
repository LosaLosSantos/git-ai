package sdk_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

func TestRepo_ResolveRevision(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.commits.revisions = map[string]string{"main": "commit-main"}
	repo := dialFake(t, fakes.register).Repo("repo")

	t.Run("found", func(t *testing.T) {
		id, err := repo.ResolveRevision(t.Context(), "main")
		require.NoError(t, err)
		require.Equal(t, "commit-main", id)
	})

	t.Run("unknown revision is NotFound", func(t *testing.T) {
		_, err := repo.ResolveRevision(t.Context(), "nope")
		require.Error(t, err)
		require.True(t, sdk.IsNotFound(err))
		require.Equal(t, codes.NotFound, sdk.Code(err))
	})

	t.Run("empty revision is invalid", func(t *testing.T) {
		_, err := repo.ResolveRevision(t.Context(), "")
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})
}

func TestRepo_ReadFile(t *testing.T) {
	t.Parallel()

	content := bytes.Repeat([]byte("0123456789"), 1000) // 10000 bytes

	fakes := newFakeServices()
	fakes.commits.files = map[string][]byte{
		"README.md": []byte("# hello\n"),
		"big.bin":   content,
		"empty.txt": {},
	}
	fakes.commits.dirs = map[string]bool{"docs": true}
	fakes.commits.chunkSize = 1024
	repo := dialFake(t, fakes.register).Repo("repo")

	t.Run("small file", func(t *testing.T) {
		data, err := repo.ReadFile(t.Context(), "main", "README.md")
		require.NoError(t, err)
		require.Equal(t, []byte("# hello\n"), data)
	})

	t.Run("file spanning several messages", func(t *testing.T) {
		data, err := repo.ReadFile(t.Context(), "main", "big.bin")
		require.NoError(t, err)
		require.Equal(t, content, data)
	})

	t.Run("empty file", func(t *testing.T) {
		data, err := repo.ReadFile(t.Context(), "main", "empty.txt")
		require.NoError(t, err)
		require.Empty(t, data)
	})

	t.Run("directory is rejected", func(t *testing.T) {
		_, err := repo.ReadFile(t.Context(), "main", "docs")
		require.Equal(t, codes.FailedPrecondition, sdk.Code(err))
	})

	t.Run("missing path is NotFound", func(t *testing.T) {
		_, err := repo.ReadFile(t.Context(), "main", "missing.txt")
		require.True(t, sdk.IsNotFound(err))
	})

	t.Run("missing arguments are invalid", func(t *testing.T) {
		_, err := repo.ReadFile(t.Context(), "", "README.md")
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))

		_, err = repo.ReadFile(t.Context(), "main", "")
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})
}

func TestRepo_ListTree(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.objects.tree = []*gitalypb.GetTreeResponse_Entry{
		{Path: []byte("README.md"), Oid: "b1", Type: gitalypb.ObjectType_BLOB, Mode: 0o100644, Size: 7},
		{Path: []byte("docs"), Oid: "t1", Type: gitalypb.ObjectType_TREE, Mode: 0o40000},
		{Path: []byte("docs/index.md"), Oid: "b2", Type: gitalypb.ObjectType_BLOB, Mode: 0o100644, Size: 42},
		{Path: []byte("docs/deep"), Oid: "t2", Type: gitalypb.ObjectType_TREE, Mode: 0o40000},
		{Path: []byte("docs/deep/x.txt"), Oid: "b3", Type: gitalypb.ObjectType_BLOB, Mode: 0o100755, Size: 3},
		{Path: []byte("vendor"), Oid: "c1", Type: gitalypb.ObjectType_COMMIT, Mode: 0o160000},
	}
	repo := dialFake(t, fakes.register).Repo("repo")

	t.Run("root, not recursive", func(t *testing.T) {
		entries, err := repo.ListTree(t.Context(), "main", "", false)
		require.NoError(t, err)
		require.Equal(t, []sdk.TreeEntry{
			{Path: "README.md", OID: "b1", Type: sdk.ObjectBlob, Mode: 0o100644, Size: 7},
			{Path: "docs", OID: "t1", Type: sdk.ObjectTree, Mode: 0o40000},
			{Path: "vendor", OID: "c1", Type: sdk.ObjectCommit, Mode: 0o160000},
		}, entries)
		require.True(t, entries[1].IsDir())
		require.False(t, entries[0].IsDir())
	})

	t.Run("subdirectory, recursive, paths relative to the listed tree", func(t *testing.T) {
		entries, err := repo.ListTree(t.Context(), "main", "docs", true)
		require.NoError(t, err)
		require.Equal(t, []string{"index.md", "deep", "deep/x.txt"}, paths(entries))
	})

	t.Run("requests include sizes and are sent to the revision", func(t *testing.T) {
		_, err := repo.ListTree(t.Context(), "main", "docs", true)
		require.NoError(t, err)

		last := fakes.objects.getTreeRequests[len(fakes.objects.getTreeRequests)-1]
		require.True(t, last.GetIncludeSizes())
		require.True(t, last.GetRecursive())
		require.Equal(t, []byte("main"), last.GetRevision())
		require.Equal(t, []byte("docs"), last.GetPath())
	})

	t.Run("empty revision is invalid", func(t *testing.T) {
		_, err := repo.ListTree(t.Context(), "", "", false)
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})
}

func paths(entries []sdk.TreeEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}
