package operations

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// unbatchedLandFixture is a repository whose main branch is at base, with a change based on the
// parent of base, served by an operations service without batching (not standalone).
func unbatchedLandFixture(t *testing.T, ctx context.Context) (context.Context, stepperFixture, func(git.ObjectID) error) {
	t.Helper()

	ctx, cfg, client := setupOperationsService(t, ctx)
	f := newStepperFixture(t, ctx, cfg)

	land := func(commit git.ObjectID) error {
		resp, err := client.UserLand(ctx, landRequest(f.repoProto, git.DefaultBranch, commit))
		if err != nil {
			return err
		}
		require.Equal(t, int32(1), resp.GetBatchSize(), "the land is not batched")
		return nil
	}

	return ctx, f, land
}

// TestUserLand_unbatchedLandUsesTheLandingAttempt is not parallel: it replaces newBatchRebaser. An
// unbatched land decides and rebases like a batch attempt (one replay through the rebaser of the
// attempt) and publishes its objects as one pack: the rebased commit is not a loose object of the
// repository, and the repository is connected.
func TestUserLand_unbatchedLandUsesTheLandingAttempt(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	var replays atomic.Int64
	original := newBatchRebaser
	newBatchRebaser = func(repo *localrepo.Repo) batchRebaser {
		return countingRebaser{batchRebaser: original(repo), rebases: &replays}
	}
	t.Cleanup(func() { newBatchRebaser = original })

	_, f, land := unbatchedLandFixture(t, ctx)
	packsBefore := len(packIndexes(t, filepath.Join(f.repoPath, "objects")))

	require.NoError(t, land(f.diverged1))
	require.Equal(t, int64(1), replays.Load(), "the diverged change is replayed once, through the rebaser")

	tip := gittest.ResolveRevision(t, f.cfg, f.repoPath, "refs/heads/"+git.DefaultBranch)
	require.NotEqual(t, f.diverged1, tip, "the change was rebased")
	require.NoFileExists(t, filepath.Join(f.repoPath, "objects", tip.String()[:2], tip.String()[2:]),
		"the rebased commit is not a loose object of the repository")
	require.Len(t, packIndexes(t, filepath.Join(f.repoPath, "objects")), packsBefore+1, "the land publishes one pack")
	gittest.Exec(t, f.cfg, "-C", f.repoPath, "fsck", "--connectivity-only", "--no-dangling")

	// A fast-forward writes no object, so it replays nothing and publishes no pack.
	forward := gittest.WriteCommit(t, f.cfg, f.repoPath, gittest.WithParents(tip), gittest.WithMessage("forward"))
	require.NoError(t, land(forward))
	require.Equal(t, int64(1), replays.Load())
	require.Len(t, packIndexes(t, filepath.Join(f.repoPath, "objects")), packsBefore+1)
	requireBranchAt(t, f.cfg, f.repoPath, git.DefaultBranch, forward)
}

// TestUserLand_unbatchedPackFailureFailsTheLand is not parallel: it replaces packBatchObjects. A land
// whose objects cannot be packed fails with Internal and leaves the branch where it was.
func TestUserLand_unbatchedPackFailureFailsTheLand(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	errPack := errors.New("injected pack failure")
	original := packBatchObjects
	packBatchObjects = func(context.Context, *localrepo.Repo, string) (int, error) { return 0, errPack }
	t.Cleanup(func() { packBatchObjects = original })

	_, f, land := unbatchedLandFixture(t, ctx)

	err := land(f.diverged1)
	testhelper.RequireGrpcCode(t, err, codes.Internal)
	require.ErrorContains(t, err, errPack.Error())
	requireBranchAt(t, f.cfg, f.repoPath, git.DefaultBranch, f.base)
}
