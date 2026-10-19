package operations

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// commitFiles sends one UserCommitFiles request with header and requests, and returns its result.
func commitFiles(t *testing.T, ctx context.Context, client gitalypb.OperationServiceClient, header *gitalypb.UserCommitFilesRequest, requests ...*gitalypb.UserCommitFilesRequest) (*gitalypb.UserCommitFilesResponse, error) {
	t.Helper()

	stream, err := client.UserCommitFiles(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(header))
	for _, request := range requests {
		require.NoError(t, stream.Send(request))
	}
	return stream.CloseAndRecv()
}

// lsTree lists the tree of commit recursively as `<mode> <path>` lines.
func lsTree(t *testing.T, cfg config.Cfg, repoPath, commit string) string {
	t.Helper()
	return string(gittest.Exec(t, cfg, "-C", repoPath, "ls-tree", "-r", "--format=%(objectmode) %(path)", commit))
}

// TestUserCommitFilesScenarios runs requests whose actions depend on each other, which the
// differential test cannot see: its two trees share the semantics of the actions.
func TestUserCommitFilesScenarios(t *testing.T) {
	t.Parallel()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	ctx, cfg, client := setupOperationsService(t, ctx)

	setup := func(t *testing.T) (*gitalypb.Repository, string) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"),
			gittest.WithTreeEntries(gittest.TreeEntry{Path: "a.txt", Mode: "100644", Content: "a\n"}))
		return repo, repoPath
	}

	t.Run("a move, then a chmod of the moved file", func(t *testing.T) {
		repo, repoPath := setup(t)
		resp, err := commitFiles(t, ctx, client, headerRequest(repo, gittest.TestUser, "master", []byte("message"), "", ""),
			moveFileHeaderRequest("a.txt", "b.txt", true),
			chmodFileHeaderRequest("b.txt", true))
		require.NoError(t, err)
		require.Equal(t, "100755 b.txt\n", lsTree(t, cfg, repoPath, resp.GetBranchUpdate().GetCommitId()))
	})

	t.Run("a move, then an update of the moved file", func(t *testing.T) {
		repo, repoPath := setup(t)
		resp, err := commitFiles(t, ctx, client, headerRequest(repo, gittest.TestUser, "master", []byte("message"), "", ""),
			moveFileHeaderRequest("a.txt", "b.txt", true),
			updateFileHeaderRequest("b.txt"), actionContentRequest("updated\n"))
		require.NoError(t, err)
		commitID := resp.GetBranchUpdate().GetCommitId()
		require.Equal(t, "100644 b.txt\n", lsTree(t, cfg, repoPath, commitID))
		require.Equal(t, "updated\n", string(gittest.Exec(t, cfg, "-C", repoPath, "show", commitID+":b.txt")))
	})

	t.Run("a move, then a create onto the moved path", func(t *testing.T) {
		// The moved file exists: the create is refused, where it used to replace it silently.
		repo, _ := setup(t)
		_, err := commitFiles(t, ctx, client, headerRequest(repo, gittest.TestUser, "master", []byte("message"), "", ""),
			moveFileHeaderRequest("a.txt", "b.txt", true),
			createFileHeaderRequest("b.txt"), actionContentRequest("created\n"))
		testhelper.RequireGrpcError(t, structuredIndexError(t, &indexError{path: "b.txt", errorType: errFileExists}), err)
	})

	t.Run("a file below a file names the file in the way", func(t *testing.T) {
		repo, _ := setup(t)
		_, err := commitFiles(t, ctx, client, headerRequest(repo, gittest.TestUser, "master", []byte("message"), "", ""),
			createFileHeaderRequest("a.txt/b/c.txt"), actionContentRequest("c\n"))
		testhelper.RequireGrpcError(t, structuredIndexError(t, &indexError{path: "a.txt", errorType: errFileExists}), err)
	})

	t.Run("a chmod of a directory", func(t *testing.T) {
		// Refused whatever the requested mode, also to clear the executable bit: a directory
		// has no executable mode (PLAN9 §15.3).
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"),
			gittest.WithTreeEntries(gittest.TreeEntry{Path: "dir", Mode: "040000", OID: gittest.WriteTree(t, cfg, repoPath,
				[]gittest.TreeEntry{{Path: "f", Mode: "100644", Content: "f\n"}})}))
		for _, executable := range []bool{true, false} {
			_, err := commitFiles(t, ctx, client, headerRequest(repo, gittest.TestUser, "master", []byte("message"), "", ""),
				chmodFileHeaderRequest("dir", executable))
			testhelper.RequireGrpcError(t, structuredIndexError(t, &indexError{path: "dir", errorType: errFileNotFound}), err)
		}
	})
}

// TestUserCommitFilesLegacyModes commits into a directory that holds entries with modes git no
// longer writes (100664, 100600, 120777): the directory is written back with their canonical
// modes, as before WP96, instead of a tree that hash-object refuses.
func TestUserCommitFilesLegacyModes(t *testing.T) {
	t.Parallel()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	ctx, cfg, client := setupOperationsService(t, ctx)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	blob := gittest.WriteBlob(t, cfg, repoPath, []byte("legacy\n"))
	legacy := map[string]string{"group-writable": "100664", "private": "100600", "link": "120777"}
	var entries []*localrepo.TreeEntry
	for name, mode := range legacy {
		canonical := "100644"
		if mode == "120777" {
			canonical = "120000"
		}
		entries = append(entries, &localrepo.TreeEntry{Path: name, Mode: canonical, Type: localrepo.Blob, OID: blob})
	}
	dir := writeLiteralTree(t, cfg, repoPath, entries, func(entry *localrepo.TreeEntry) string { return legacy[entry.Path] })
	root := writeLiteralTree(t, cfg, repoPath, []*localrepo.TreeEntry{{Path: "dir", Mode: "040000", Type: localrepo.Tree, OID: dir}},
		func(*localrepo.TreeEntry) string { return "40000" })
	parent := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTree(root), gittest.WithBranch("master"))
	// The raw tree holds the legacy mode (cat-file -p would print it canonical).
	require.Contains(t, string(gittest.Exec(t, cfg, "-C", repoPath, "cat-file", "tree", dir.String())), "100664 group-writable")

	resp, err := commitFiles(t, ctx, client, headerRequest(repo, gittest.TestUser, "master", []byte("message"), "", ""),
		createFileHeaderRequest("dir/new"), actionContentRequest("new\n"))
	require.NoError(t, err)

	commitID := resp.GetBranchUpdate().GetCommitId()
	require.Equal(t, parent.String(), strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "log", "-1", "--format=%P", commitID))))
	listed := strings.Split(strings.TrimSuffix(lsTree(t, cfg, repoPath, commitID), "\n"), "\n")
	sort.Strings(listed)
	require.Equal(t, []string{
		"100644 dir/group-writable",
		"100644 dir/new",
		"100644 dir/private",
		"120000 dir/link",
	}, listed)
	// The rewritten directory holds the canonical modes.
	rewritten := string(gittest.Exec(t, cfg, "-C", repoPath, "cat-file", "tree", commitID+":dir"))
	require.Contains(t, rewritten, "100644 group-writable")
	require.NotContains(t, rewritten, "100664")
	require.NotContains(t, rewritten, "120777")
}

// TestUserCommitFilesFetchesNestedTrees takes the parent from a start repository whose commit is
// missing locally: the commit and its nested trees are fetched into the quarantine after the
// request's object reader started, and the lazy tree reads them through it.
func TestUserCommitFilesFetchesNestedTrees(t *testing.T) {
	t.Parallel()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	ctx, cfg, client := setupOperationsService(t, ctx)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"))

	startRepo, startRepoPath := gittest.CreateRepository(t, ctx, cfg)
	inner := gittest.WriteTree(t, cfg, startRepoPath, []gittest.TreeEntry{{Path: "c.txt", Mode: "100644", Content: "c\n"}})
	middle := gittest.WriteTree(t, cfg, startRepoPath, []gittest.TreeEntry{{Path: "b", Mode: "040000", OID: inner}})
	startCommit := gittest.WriteCommit(t, cfg, startRepoPath, gittest.WithBranch("main"),
		gittest.WithTreeEntries(gittest.TreeEntry{Path: "a", Mode: "040000", OID: middle}))

	header := headerRequest(repo, gittest.TestUser, "new", []byte("message"), "main", "")
	setStartRepository(header, startRepo)
	resp, err := commitFiles(t, ctx, client, header, createFileHeaderRequest("a/b/new.txt"), actionContentRequest("new\n"))
	require.NoError(t, err)

	commitID := resp.GetBranchUpdate().GetCommitId()
	require.Equal(t, startCommit.String(), strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "log", "-1", "--format=%P", commitID))))
	require.Equal(t, "100644 a/b/c.txt\n100644 a/b/new.txt\n", lsTree(t, cfg, repoPath, commitID))
	require.Equal(t, commitID, strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "refs/heads/new"))))
}
