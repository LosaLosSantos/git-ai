package operations

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

func TestUserSquash_mergeErrorIsReturned(t *testing.T) {
	t.Parallel()

	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, testUserSquashMergeErrorIsReturned)
}

// testUserSquashMergeErrorIsReturned squashes onto a start commit whose merge with the end commit
// fails without a conflict: both changed the file a, and the end commit's blob of it does not exist,
// so git merge-tree cannot merge the contents. The RPC fails. It used to drop the error and return a
// squash commit ID that is empty, after voting on it and migrating the quarantine.
func testUserSquashMergeErrorIsReturned(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "a", Mode: "100644", Content: "base\n"},
	))
	start := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "a", Mode: "100644", Content: "start\n"},
	))

	// The blob is only hashed, never written, so the tree of the end commit refers to a missing
	// object.
	missingBlob := strings.TrimSpace(string(gittest.ExecOpts(t, cfg,
		gittest.ExecConfig{Stdin: strings.NewReader("end, never written\n")},
		"-C", repoPath, "hash-object", "--stdin")))
	tree := strings.TrimSpace(string(gittest.ExecOpts(t, cfg,
		gittest.ExecConfig{Stdin: strings.NewReader(fmt.Sprintf("100644 blob %s\ta\n", missingBlob))},
		"-C", repoPath, "mktree", "--missing")))
	end := git.ObjectID(strings.TrimSpace(string(gittest.Exec(t, cfg,
		"-C", repoPath, "commit-tree", tree, "-p", base.String(), "-m", "end"))))

	response, err := client.UserSquash(ctx, &gitalypb.UserSquashRequest{
		Repository:    repoProto,
		User:          gittest.TestUser,
		Author:        author,
		CommitMessage: commitMessage,
		StartSha:      start.String(),
		EndSha:        end.String(),
	})
	testhelper.RequireGrpcCode(t, err, codes.Internal)
	require.Empty(t, response.GetSquashSha())
}
