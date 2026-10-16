package e2e

// Spec tests for WP39 (phase-4 e2e), written from PLAN4.md. The land tests run
// against a standalone server (startGitaly), because only a standalone server
// batches its lands: a plain testcfg.Build configuration takes the unbatched
// land path, which would satisfy the assertions without exercising batching.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/landlane"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/sdk"
)

func TestSpecPhase4ConfigDefaults(t *testing.T) {
	cfg := testcfg.Build(t)

	// Foundation rule: graph_ancestry and commit_graph default to false.
	require.False(t, cfg.Graft.Land.GraphAncestry, "graph_ancestry must default to false")
	require.False(t, cfg.Graft.Derived.CommitGraph, "commit_graph must default to false")

	// Empty target_latency means DefaultGraftLandBatchTargetLatency (500ms).
	d, err := config.GraftLandBatch{}.TargetLatencyOrDefault()
	require.NoError(t, err)
	require.Equal(t, 500*time.Millisecond, d)
}

func TestSpecTargetLatencyZeroDisables(t *testing.T) {
	// "0s" disables adaptation: the helper returns a zero duration.
	d, err := config.GraftLandBatch{TargetLatency: "0s"}.TargetLatencyOrDefault()
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), d)
}

func TestSpecTargetLatencyInvalidIsError(t *testing.T) {
	// Target latency is a Go duration; anything else must be rejected.
	_, err := config.GraftLandBatch{TargetLatency: "soon"}.TargetLatencyOrDefault()
	require.Error(t, err)
}

func TestSpecCompleteEarlyOutsideExecutorReturnsFalse(t *testing.T) {
	// CompleteEarly returns false when ctx is not an executor context of the
	// landlane package. A plain test context is never one.
	ctx := testhelper.Context(t)
	var r landlane.Result[string]
	require.False(t, landlane.CompleteEarly(ctx, 0, r))
}

func TestSpecCompleteEarlyIndexOutOfRangeReturnsFalse(t *testing.T) {
	// Negative indexes are out of range; CompleteEarly must return false.
	var r landlane.Result[string]
	require.False(t, landlane.CompleteEarly(context.Background(), -1, r))
}

// specBatchedRun lands one disjoint change per worker on main at the same
// time, on a standalone server whose lanes collect them for window. It returns
// the outcome of each worker, the largest batch size any land reported, and
// the paths that the workers added to main.
func specBatchedRun(t *testing.T, graphAncestry bool, workers int, window string) ([]string, int32, []string) {
	t.Helper()

	graft, tokens := workerGraft(workers)
	graft.Land.Batch.Window = window
	graft.Land.GraphAncestry = graphAncestry

	s := startGitaly(t, graft, sharedTestToken)
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{"README.md": "phase 4 spec\n"})
	repos := make([]*sdk.Repo, workers)
	commits := make([]string, workers)
	for i := range repos {
		name := workerName(i)
		repos[i] = s.repoAs(t, tokens[name])
		var err error
		commits[i], err = commitFile(ctx, repos[i], name, "topic/spec-"+name, base, fmt.Sprintf("spec/%s.txt", name), name+"\n")
		require.NoError(t, err)
	}

	outcomes := make([]string, workers)
	results := make([]*sdk.LandResult, workers)
	var wg sync.WaitGroup
	for i := range repos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := landOn(ctx, repos[i], "main", commits[i], false)
			if err != nil {
				outcomes[i] = "error: " + sdk.Code(err).String()
				return
			}
			outcomes[i] = "landed"
			results[i] = res
		}(i)
	}
	wg.Wait()

	var largest int32
	tip := s.git(t, "rev-parse", "refs/heads/main")
	members := landBatchMembers(ctx, t, repos[0])
	for i, res := range results {
		if res == nil {
			continue
		}
		largest = max(largest, res.Batch.Size)
		s.git(t, "merge-base", "--is-ancestor", res.LandedCommitID, tip)
		require.Equal(t, workerName(i), members[commits[i]], "the landed change of %s is a batch member attributed to it", workerName(i))
	}

	files := strings.Split(s.git(t, "ls-tree", "-r", "--name-only", "main", "spec"), "\n")
	sort.Strings(files)
	return outcomes, largest, files
}

func TestSpecPhase4GraphAncestryOnOffIdentical(t *testing.T) {
	// Batched lands with GraphAncestry on produce the same outcome as off:
	// the same per-member classification and the same files on main. The
	// lands are submitted at once to a standalone server with a long batch
	// window, so they are batched together.
	t.Parallel()

	const workers = 3
	offOutcomes, offBatch, offFiles := specBatchedRun(t, false, workers, "300ms")
	onOutcomes, onBatch, onFiles := specBatchedRun(t, true, workers, "300ms")

	require.Greater(t, offBatch, int32(1), "the lands without graph ancestry are batched")
	require.Greater(t, onBatch, int32(1), "the lands with graph ancestry are batched")
	require.Equal(t, offOutcomes, onOutcomes, "GraphAncestry on must not change outcomes")
	require.Equal(t, offFiles, onFiles, "GraphAncestry on must not change the files on main")
	for i, o := range onOutcomes {
		require.Equal(t, "landed", o, "non-conflicting member %d must land", i)
	}
	require.Len(t, onFiles, workers, "every disjoint change is on main")
}

func TestSpecPhase4BatchOfOneKeepsMember(t *testing.T) {
	// When exactly one member changes the branch the batch is recorded as a
	// `land` changeset with the member's real actor, and it still carries that
	// single member (original -> landed).
	t.Parallel()

	graft, tokens := workerGraft(1)
	s := startGitaly(t, graft, sharedTestToken)
	ctx := s.ctx

	const agent = "worker-00"
	require.Equal(t, agent, workerName(0))
	repo := s.repoAs(t, tokens[agent])

	base := writeMain(t, s, map[string]string{"README.md": "solo\n"})
	commit, err := commitFile(ctx, repo, agent, "topic/spec-solo", base, "solo.txt", "solo\n")
	require.NoError(t, err)

	res, err := landOn(ctx, repo, "main", commit, false)
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Batch.Size, "the change is a batch of one")
	require.Equal(t, "1", s.git(t, "rev-list", "--count", base+"..refs/heads/main"), "exactly one commit is added")
	require.Equal(t, res.LandedCommitID, s.git(t, "rev-parse", "refs/heads/main"), "main is at the landed commit")

	var lands []*sdk.Changeset
	for _, cs := range allChangesets(ctx, t, repo) {
		if string(cs.GetReference()) != "refs/heads/main" {
			continue
		}
		require.NotEqual(t, landBatchOperation, cs.GetOperation(), "a batch of one is not a land_batch")
		lands = append(lands, cs)
	}
	require.Len(t, lands, 1, "exactly one changeset records the land")

	cs := lands[0]
	require.Equal(t, "land", cs.GetOperation(), "a batch of one is a land")
	require.Equal(t, agent, cs.GetActor(), "the actor is the member's real actor")
	require.Len(t, cs.GetMembers(), 1, "the land keeps its single member, so it went through the batch path")

	member := cs.GetMembers()[0]
	require.Equal(t, commit, member.GetOriginalCommitId(), "the member is the submitted commit")
	require.Equal(t, res.LandedCommitID, member.GetCommitId(), "the member is the landed commit")
	require.Equal(t, agent, member.GetActor(), "the member is attributed to its agent")
	require.Equal(t, map[string]string{commit: agent}, landBatchMembers(ctx, t, repo), "the batch of one is the only batch member, attributed to its agent")
}
