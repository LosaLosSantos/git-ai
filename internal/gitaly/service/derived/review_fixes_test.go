package derived

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

func TestDerivedRPCs_NULPathIsInvalidArgument(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, svc := setupServices(t, testcfg.Build(t))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	writeHistory(t, cfg, repoPath, mergeHistory())

	path := []byte("a\x00b")

	_, err := drainPathHistory(ctx, svc.derived, &gitalypb.PathHistoryRequest{
		Repository: repoProto,
		Revision:   []byte("root"),
		Path:       path,
	})
	testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)

	_, err = drainBlame(ctx, svc.derived, &gitalypb.BlameRequest{
		Repository: repoProto,
		Revision:   []byte("root"),
		Path:       path,
	})
	testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
}

func TestLastCommitForPath_ShallowRepositoryMatchesGit(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, enabled := setupServices(t, testcfg.Build(t))
	disabled := disabledServices(t, cfg)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	ids := writeHistory(t, cfg, repoPath, linearHistory(3, func(i int) string {
		if i == 0 {
			return "target"
		}
		return ""
	}))

	// Git treats the shallow commit c1 as the root of the history. The last commit that
	// changed target at c2 is therefore c1, although target was added in c0.
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "shallow"), []byte(ids["c1"].String()+"\n"), mode.File))
	require.Equal(t, ids["c1"].String(), gitLastMod(t, cfg, repoPath, ids["c2"], "target"))

	for _, svc := range []services{enabled, disabled} {
		response, err := svc.commit.LastCommitForPath(ctx, &gitalypb.LastCommitForPathRequest{
			Repository: repoProto,
			Revision:   []byte("c2"),
			Path:       []byte("target"),
		})
		require.NoError(t, err)
		require.Equal(t, ids["c1"].String(), response.GetCommit().GetId())
	}
}
