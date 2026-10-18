package operations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// unbatchedLandFixture is a repository whose main branch is at base, with changes based on the
// parent of base, served by an operations service without batching (not standalone).
func unbatchedLandFixture(t *testing.T, ctx context.Context) (context.Context, stepperFixture, gitalypb.OperationServiceClient) {
	t.Helper()

	ctx, cfg, client := setupOperationsService(t, ctx)
	return ctx, newStepperFixture(t, ctx, cfg), client
}

// landUnbatched lands commit on the main branch of f and requires that it did not share a batch.
func landUnbatched(t *testing.T, ctx context.Context, client gitalypb.OperationServiceClient, f stepperFixture, commit git.ObjectID) (*gitalypb.UserLandResponse, error) {
	t.Helper()

	resp, err := client.UserLand(ctx, landRequest(f.repoProto, git.DefaultBranch, commit))
	if err == nil {
		require.Equal(t, int32(1), resp.GetBatchSize(), "the land is not batched")
	}
	return resp, err
}

// TestUserLand_unbatchedLandPublishesLooseObjects is not parallel: it replaces newBatchRebaser and
// packBatchObjects. An unbatched land decides and rebases like a batch attempt (one replay through
// the rebaser of the attempt), in the loose publish mode: its rebaser writes with the server's fsync
// configuration, nothing is packed, and the migration moves its loose objects into the repository.
func TestUserLand_unbatchedLandPublishesLooseObjects(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	var replays atomic.Int64
	var modes sync.Map
	originalRebaser := newBatchRebaser
	newBatchRebaser = func(repo *localrepo.Repo, mode landPublishMode) batchRebaser {
		modes.Store(mode, true)
		return countingRebaser{batchRebaser: originalRebaser(repo, mode), rebases: &replays}
	}
	t.Cleanup(func() { newBatchRebaser = originalRebaser })

	originalPack := packBatchObjects
	packBatchObjects = func(context.Context, *localrepo.Repo, string) (int, error) {
		return 0, errors.New("an unbatched land must not pack")
	}
	t.Cleanup(func() { packBatchObjects = originalPack })

	ctx, f, client := unbatchedLandFixture(t, ctx)
	packsBefore := len(packIndexes(t, filepath.Join(f.repoPath, "objects")))

	_, err := landUnbatched(t, ctx, client, f, f.diverged1)
	require.NoError(t, err)
	require.Equal(t, int64(1), replays.Load(), "the diverged change is replayed once, through the rebaser")

	_, loose := modes.Load(landPublishLoose)
	_, pack := modes.Load(landPublishPack)
	require.True(t, loose, "the rebaser of an unbatched land is in the loose mode")
	require.False(t, pack, "an unbatched land never uses the pack mode")

	tip := gittest.ResolveRevision(t, f.cfg, f.repoPath, "refs/heads/"+git.DefaultBranch)
	require.NotEqual(t, f.diverged1, tip, "the change was rebased")
	require.FileExists(t, filepath.Join(f.repoPath, "objects", tip.String()[:2], tip.String()[2:]),
		"the rebased commit is a loose object of the repository")
	require.Len(t, packIndexes(t, filepath.Join(f.repoPath, "objects")), packsBefore, "an unbatched land publishes no pack")
	gittest.Exec(t, f.cfg, "-C", f.repoPath, "fsck", "--connectivity-only", "--no-dangling")
}

// TestUserLand_contendedUnbatchedLandsLeaveNoPacks: unbatched lands that race on one branch lose
// compare-and-swaps and retry, and no attempt, lost or won, leaves a pack in the repository.
func TestUserLand_contendedUnbatchedLandsLeaveNoPacks(t *testing.T) {
	t.Parallel()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	ctx, f, client := unbatchedLandFixture(t, ctx)
	packsBefore := len(packIndexes(t, filepath.Join(f.repoPath, "objects")))

	rootEntry := gittest.TreeEntry{Mode: "100644", Path: "root", Content: "root"}
	root := gittest.ResolveRevision(t, f.cfg, f.repoPath, f.base.String()+"^")
	const changes = 16
	commits := make([]git.ObjectID, changes)
	for i := range commits {
		path := fmt.Sprintf("change-%d", i)
		commits[i] = gittest.WriteCommit(t, f.cfg, f.repoPath, gittest.WithParents(root), gittest.WithMessage(path),
			gittest.WithTreeEntries(rootEntry, gittest.TreeEntry{Mode: "100644", Path: path, Content: path}))
	}

	attempts := make([]int32, changes)
	errs := make([]error, changes)
	var wg sync.WaitGroup
	for i := range commits {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := landUnbatched(t, ctx, client, f, commits[i])
			errs[i] = err
			attempts[i] = resp.GetAttempts()
		}(i)
	}
	wg.Wait()

	retried := 0
	for i := range commits {
		require.NoError(t, errs[i], "land %d", i)
		if attempts[i] > 1 {
			retried++
		}
	}
	require.Positive(t, retried, "the lands raced: some lost a compare-and-swap and retried")

	tree := gittest.Exec(t, f.cfg, "-C", f.repoPath, "ls-tree", "--name-only", "refs/heads/"+git.DefaultBranch)
	for i := range commits {
		require.Contains(t, string(tree), fmt.Sprintf("change-%d", i))
	}
	require.Len(t, packIndexes(t, filepath.Join(f.repoPath, "objects")), packsBefore, "no attempt left a pack")
	gittest.Exec(t, f.cfg, "-C", f.repoPath, "fsck", "--connectivity-only")
}

// helperFsyncConfig returns the effective core.fsync of the running git processes whose command
// line holds every one of needles: the last -c core.fsync=... wins, as for git.
func helperFsyncConfig(t *testing.T, needles ...string) []string {
	t.Helper()

	procs, err := filepath.Glob("/proc/[0-9]*/cmdline")
	require.NoError(t, err)

	var values []string
	for _, proc := range procs {
		raw, err := os.ReadFile(proc)
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		line := strings.Join(args, " ")

		match := true
		for _, needle := range needles {
			if !strings.Contains(line, needle) {
				match = false
				break
			}
		}
		if !match {
			continue
		}

		value := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-c" {
				if v, ok := strings.CutPrefix(args[i+1], "core.fsync="); ok {
					value = v
				}
			}
		}
		values = append(values, value)
	}
	return values
}

// TestLandingAttemptModesSetTheHelperFsync checks the command lines of the helper processes that an
// attempt starts: in the pack mode the merge-tree and hash-object helpers run with core.fsync=none,
// and in the loose mode with the server's configuration, which fsyncs objects.
func TestLandingAttemptModesSetTheHelperFsync(t *testing.T) {
	t.Parallel()

	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	for _, tc := range []struct {
		mode landPublishMode
		want func(t *testing.T, value string)
	}{
		{mode: landPublishPack, want: func(t *testing.T, value string) { require.Equal(t, "none", value) }},
		{mode: landPublishLoose, want: func(t *testing.T, value string) { require.Contains(t, value, "objects") }},
	} {
		t.Run(fmt.Sprint(tc.mode), func(t *testing.T) {
			cfg := testcfg.Build(t)
			srv, cfg := runLandBatchStepperServer(t, cfg)
			f := newStepperFixture(t, ctx, cfg)

			attempt, cleanup, err := srv.newLandingAttempt(ctx, f.repoProto, tc.mode, ignoreLandPhase)
			require.NoError(t, err)
			defer cleanup()

			// A rebase starts both helpers, which run until the attempt ends.
			step, err := attempt.decide(ctx, f.base, f.request(t, ctx, f.diverged1))
			require.NoError(t, err)
			require.True(t, step.rewritten)

			for _, helper := range []string{"merge-tree", "hash-object"} {
				values := helperFsyncConfig(t, f.repoPath, helper, "--stdin")
				require.Len(t, values, 1, "one %s helper of the attempt is running", helper)
				tc.want(t, values[0])
			}
		})
	}
}
