package operations

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestStandalone_MissingUserIsNotCreated checks that a standalone Graft server
// does not invent a User for requests that write commits or annotated tags.
// Such objects would carry an empty name and email, so the handler has to
// reject the request as it does without Graft.
func TestStandalone_MissingUserIsNotCreated(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{Graft: config.Graft{Standalone: true}}))
	ctx, cfg, client := setupOperationsServiceWithCfg(t, ctx, cfg)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents())

	t.Run("annotated tag without user is rejected", func(t *testing.T) {
		_, err := client.UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{
			Repository:     repoProto,
			TagName:        []byte("v1"),
			TargetRevision: []byte(commitID),
			Message:        []byte("annotated"),
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "error: %v", err)

		// No tag object with an empty tagger may have been written.
		require.Empty(t, gittest.Exec(t, cfg, "-C", repoPath, "tag", "--list"))
	})

	t.Run("annotated tag with user is created", func(t *testing.T) {
		_, err := client.UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{
			Repository:     repoProto,
			TagName:        []byte("v2"),
			TargetRevision: []byte(commitID),
			Message:        []byte("annotated"),
			User:           gittest.TestUser,
		})
		require.NoError(t, err)
	})
}
