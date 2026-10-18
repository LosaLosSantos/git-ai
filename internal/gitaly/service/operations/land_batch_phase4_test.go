package operations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// orderedStepper records the update of a batch in the events of the test, so that they can be
// ordered against the early deliveries.
type orderedStepper struct {
	*fakeLandStepper
	events *[]string
}

func (s orderedStepper) Update(ctx context.Context, reference git.ReferenceName, members []changelog.Member, rewritten bool, old, newTip git.ObjectID) (landUpdateKind, error) {
	*s.events = append(*s.events, "update")
	return s.fakeLandStepper.Update(ctx, reference, members, rewritten, old, newTip)
}

// slowUpdateStepper fails its update unless the early delivery of the test happened first.
type slowUpdateStepper struct {
	*fakeLandStepper
	delivered <-chan struct{}
}

func (s slowUpdateStepper) Update(ctx context.Context, reference git.ReferenceName, members []changelog.Member, rewritten bool, old, newTip git.ObjectID) (landUpdateKind, error) {
	select {
	case <-s.delivered:
	case <-time.After(10 * time.Second):
		return landUpdateFailed, errors.New("the early outcome was not delivered before the update")
	}

	return s.fakeLandStepper.Update(ctx, reference, members, rewritten, old, newTip)
}

func TestLandBatchExecutorDeliversFinalOutcomesEarly(t *testing.T) {
	t.Parallel()

	var events []string
	fake := &fakeLandStepper{
		tip: "main0",
		landErr: map[string]error{
			"bad":    structerr.NewInvalidArgument("bad commit"),
			"denied": structerr.NewPermissionDenied("denied by policy"),
		},
		landed: map[string]bool{"old": true, "old2": true},
	}

	executor := newFakeLandExecutor(fake, 3)
	executor.newStepper = func(context.Context) (landBatchStepper, func(), error) {
		return orderedStepper{fakeLandStepper: fake, events: &events}, func() {}, nil
	}
	executor.early = func(_ context.Context, index int, outcome landBatchOutcome) {
		events = append(events, fmt.Sprintf("early:%d", index))
		require.Equal(t, 1, outcome.Attempts, "an early outcome reports the attempt that decided it")
	}

	// "bad" and "old" are decided before any member. "denied" is final although it follows the member
	// "c1", and "old2" is already landed only on top of the member, so it waits for the update.
	outcomes := executor.Execute(context.Background(), testLandRequests(false, "bad", "old", "c1", "denied", "old2"))

	require.Equal(t, []string{"early:0", "early:1", "early:3", "update"}, events)

	require.Error(t, outcomes[0].Err)
	require.True(t, outcomes[1].AlreadyLanded)
	require.NoError(t, outcomes[2].Err)
	require.Equal(t, "c1", outcomes[2].LandedCommit.String())
	require.Error(t, outcomes[3].Err)
	require.True(t, outcomes[4].AlreadyLanded)
}

func TestLandBatchExecutorEarlyDeliveryIsOptional(t *testing.T) {
	t.Parallel()

	fake := &fakeLandStepper{
		tip:     "main0",
		landErr: map[string]error{"bad": structerr.NewInvalidArgument("bad commit")},
	}

	outcomes := newFakeLandExecutor(fake, 3).Execute(context.Background(), testLandRequests(false, "bad", "c1"))

	require.Error(t, outcomes[0].Err)
	require.NoError(t, outcomes[1].Err)
	require.Equal(t, []string{"c1"}, fake.updates[0].members2Commits())
}

func TestLandBatchExecutorDeliversEarlyBeforeSlowUpdate(t *testing.T) {
	t.Parallel()

	delivered := make(chan struct{})
	fake := &fakeLandStepper{
		tip:     "main0",
		landErr: map[string]error{"bad": structerr.NewInvalidArgument("bad commit")},
	}

	executor := newFakeLandExecutor(fake, 3)
	executor.newStepper = func(context.Context) (landBatchStepper, func(), error) {
		return slowUpdateStepper{fakeLandStepper: fake, delivered: delivered}, func() {}, nil
	}
	executor.early = func(_ context.Context, index int, _ landBatchOutcome) {
		if index == 0 {
			close(delivered)
		}
	}

	outcomes := executor.Execute(context.Background(), testLandRequests(false, "bad", "c1"))

	require.Error(t, outcomes[0].Err)
	require.NoError(t, outcomes[1].Err, "the update ran after the early delivery")
	require.Equal(t, "c1", outcomes[1].LandedCommit.String())
}

func TestLandBatchExecutorEarlyOutcomeIsNotLandedAgain(t *testing.T) {
	t.Parallel()

	fake := &fakeLandStepper{
		tip:     "main0",
		landErr: map[string]error{"bad": structerr.NewInvalidArgument("bad commit")},
		script:  []scriptedUpdate{{kind: landUpdateLost}},
	}

	var early []int
	executor := newFakeLandExecutor(fake, 3)
	executor.early = func(_ context.Context, index int, _ landBatchOutcome) {
		early = append(early, index)
	}

	outcomes := executor.Execute(context.Background(), testLandRequests(false, "bad", "c1"))

	// The batch is rebuilt after the lost compare-and-swap. The early request is not part of the
	// rebuilt batch, so it is landed once and its outcome does not change.
	require.Equal(t, []int{0}, early)
	require.Equal(t, 1, countLandCalls(fake.landCalls, "bad"))
	require.Len(t, fake.updates, 2)
	require.Equal(t, []string{"c1"}, fake.updates[1].members2Commits())
	require.Error(t, outcomes[0].Err)
	require.NoError(t, outcomes[1].Err)
	require.Equal(t, "c1", outcomes[1].LandedCommit.String())
}

func TestLandBatchExecutorEarlyOutcomeIsNotRetriedAfterRejection(t *testing.T) {
	t.Parallel()

	fake := &fakeLandStepper{
		tip:     "main0",
		landErr: map[string]error{"bad": structerr.NewInvalidArgument("bad commit")},
		script:  []scriptedUpdate{{kind: landUpdateRejected, err: errors.New("rejected by hook")}},
	}

	executor := newFakeLandExecutor(fake, 3)
	executor.early = func(context.Context, int, landBatchOutcome) {}

	outcomes := executor.Execute(context.Background(), testLandRequests(false, "bad", "c1"))

	// Only the request that was not delivered early lands alone after the rejection.
	require.Equal(t, []string{"c1"}, fake.fallbacks)
	require.Error(t, outcomes[0].Err)
	require.NoError(t, outcomes[1].Err)
	require.Equal(t, "c1", outcomes[1].Response.GetLandedCommitId())
}

// A conflict that follows a member depends on the member: the member may never reach the branch. Such a
// conflict is not final, so it is not delivered early, and the rejected batch lands it alone.
func TestLandBatchExecutorConflictAfterMemberIsNotEarly(t *testing.T) {
	t.Parallel()

	fake := &fakeLandStepper{
		tip:            "main0",
		conflictsOnTop: map[string]git.ObjectID{"x": "m"},
		script:         []scriptedUpdate{{kind: landUpdateRejected, err: errors.New("rejected by hook")}},
	}

	var early []int
	executor := newFakeLandExecutor(fake, 3)
	executor.early = func(_ context.Context, index int, _ landBatchOutcome) {
		early = append(early, index)
	}

	outcomes := executor.Execute(context.Background(), testLandRequests(false, "m", "x"))

	require.Empty(t, early, "the conflict depends on the member, which did not reach the branch")
	require.Equal(t, []string{"m", "x"}, fake.fallbacks, "the rejected batch lands every request alone")
	require.NoError(t, outcomes[0].Err)
	require.NoError(t, outcomes[1].Err, "x lands alone, without the member it conflicted with")
}

// A conflict decided before any member is final, as the tip it conflicts with is the branch tip.
func TestLandBatchExecutorConflictBeforeMemberIsEarly(t *testing.T) {
	t.Parallel()

	fake := &fakeLandStepper{
		tip:            "main0",
		conflictsOnTop: map[string]git.ObjectID{"x": "main0"},
	}

	var early []int
	executor := newFakeLandExecutor(fake, 3)
	executor.early = func(_ context.Context, index int, _ landBatchOutcome) {
		early = append(early, index)
	}

	outcomes := executor.Execute(context.Background(), testLandRequests(false, "x", "c1"))

	require.Equal(t, []int{0}, early)
	require.Error(t, outcomes[0].Err)
	require.NoError(t, outcomes[1].Err)
	require.Equal(t, []string{"c1"}, fake.updates[0].members2Commits())
}

func countLandCalls(calls []string, commit string) int {
	count := 0
	for _, call := range calls {
		if call == commit {
			count++
		}
	}

	return count
}

// soloFakeStepper publishes a batch of one member through UpdateSolo, and records the actor of the
// member it was given.
type soloFakeStepper struct {
	*fakeLandStepper
	solo    []string
	members []changelog.Member
}

func (s *soloFakeStepper) UpdateSolo(_ context.Context, req *landBatchRequest, member changelog.Member, _ git.ReferenceName, _ bool, old, newTip git.ObjectID) (landUpdateKind, error) {
	s.solo = append(s.solo, req.actor)
	s.members = append(s.members, member)

	if old != s.tip {
		s.tip = "external"
		return landUpdateLost, errors.New("reference moved")
	}

	s.tip = newTip
	return landUpdated, nil
}

func TestLandBatchExecutorBatchOfOnePublishesAsSoloLand(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc        string
		commits     []string
		wantSolo    []string
		wantUpdates int
	}{
		{desc: "one member is published alone", commits: []string{"c1"}, wantSolo: []string{"agent-c1"}, wantUpdates: 0},
		{desc: "two members are published as a batch", commits: []string{"c1", "c2"}, wantUpdates: 1},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			stepper := &soloFakeStepper{fakeLandStepper: &fakeLandStepper{tip: "main0"}}
			executor := newFakeLandExecutor(stepper.fakeLandStepper, 3)
			executor.newStepper = func(context.Context) (landBatchStepper, func(), error) {
				return stepper, func() {}, nil
			}

			outcomes := executor.Execute(context.Background(), testLandRequests(false, tc.commits...))

			for _, outcome := range outcomes {
				require.NoError(t, outcome.Err)
			}
			require.Equal(t, tc.wantSolo, stepper.solo)
			if len(tc.wantSolo) > 0 {
				require.Equal(t, []changelog.Member{{OriginalCommit: "c1", Commit: "c1", Actor: "agent-c1"}}, stepper.members,
					"the batch of one records its member, with its own commits and actor")
			}
			require.Len(t, stepper.updates, tc.wantUpdates)
			require.Equal(t, tc.commits[len(tc.commits)-1], stepper.tip.String())
		})
	}
}

func TestLandBatch_batchOfOneRecordsLandWithActor(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchOfOneRecordsLandWithActor)
}

func testLandBatchOfOneRecordsLandWithActor(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupAgentLandService(t, ctx, agentBatchingGraft(nil))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	change := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

	responses, errs := landInOrder(ctx, client, repoProto, agentLanding{agent: "agent-a", commit: change})
	require.NoError(t, errs[0])
	require.Equal(t, int32(1), responses[0].GetBatchSize())
	require.Equal(t, change.String(), responses[0].GetLandedCommitId())

	entries, err := changelog.List(repoPath, 0, 100)
	require.NoError(t, err)

	var updates []changelog.Entry
	for _, entry := range entries {
		if entry.Reference == git.DefaultRef.String() {
			updates = append(updates, entry)
		}
	}
	require.NotEmpty(t, updates)

	last := updates[len(updates)-1]
	require.Equal(t, changelogOperationLand, last.Operation, "a batch of one is recorded as a land, not as a land_batch")
	require.Equal(t, "agent-a", last.Actor, "the update carries the actor of the request")
	// The member maps the original commit of the request to the commit it landed as.
	require.Len(t, last.Members, 1, "a batch of one records its member")
	require.Equal(t, change.String(), last.Members[0].OriginalCommit)
	require.Equal(t, change.String(), last.Members[0].Commit)
	require.Equal(t, "agent-a", last.Members[0].Actor)
}

// An early delivery reports the batch as the responses delivered with the batch do: the size of the batch
// and the index of the request in it. The rebuild after a lost compare-and-swap does not change them.
func TestLandBatchExecutorResponsesShareBatchSizeAndPosition(t *testing.T) {
	t.Parallel()

	fake := &fakeLandStepper{
		tip:    "main0",
		landed: map[string]bool{"old": true},
		script: []scriptedUpdate{{kind: landUpdateLost}},
	}

	var early []landBatchOutcome
	executor := newFakeLandExecutor(fake, 3)
	executor.early = func(_ context.Context, _ int, outcome landBatchOutcome) {
		early = append(early, outcome)
	}

	outcomes := executor.Execute(context.Background(), testLandRequests(false, "old", "c1", "c2"))

	require.Len(t, early, 1)
	require.Equal(t, 3, early[0].BatchSize)
	require.Equal(t, 0, early[0].BatchPosition)

	require.Len(t, fake.updates, 2)
	for pos, outcome := range outcomes {
		require.NoError(t, outcome.Err, "request %d", pos)
		require.Equal(t, 3, outcome.BatchSize, "request %d", pos)
		require.Equal(t, pos, outcome.BatchPosition, "request %d", pos)
	}
	require.True(t, outcomes[0].AlreadyLanded)
}

// An invalid commit ID does not depend on the tip, so its failure is final after a member too. An invalid
// change depends on the tip, which the member changes, so it waits for the update.
func TestLandBatchExecutorInvalidAfterMemberIsEarlyOnlyWhenTipIndependent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc           string
		tipIndependent bool
		wantEarly      []int
	}{
		{desc: "an unparsable commit ID does not depend on the tip", tipIndependent: true, wantEarly: []int{1}},
		{desc: "an invalid change depends on the tip", tipIndependent: false, wantEarly: nil},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			fake := &fakeLandStepper{
				tip:            "main0",
				landErr:        map[string]error{"bad": structerr.NewInvalidArgument("bad commit")},
				tipIndependent: map[string]bool{"bad": tc.tipIndependent},
			}

			var early []int
			executor := newFakeLandExecutor(fake, 3)
			executor.early = func(_ context.Context, index int, _ landBatchOutcome) {
				early = append(early, index)
			}

			outcomes := executor.Execute(context.Background(), testLandRequests(false, "c1", "bad", "c2"))

			require.Equal(t, tc.wantEarly, early)
			require.NoError(t, outcomes[0].Err)
			require.Error(t, outcomes[1].Err)
			require.NoError(t, outcomes[2].Err)
			require.Equal(t, []string{"c1", "c2"}, fake.updates[0].members2Commits())
		})
	}
}

// countingRebaser counts the replays of the batch attempts.
type countingRebaser struct {
	batchRebaser
	rebases *atomic.Int64
}

func (r countingRebaser) Replay(ctx context.Context, upstream git.ObjectID, todo []git.ObjectID, committer *git.Signature) (git.ObjectID, error) {
	r.rebases.Add(1)
	return r.batchRebaser.Replay(ctx, upstream, todo, committer)
}

// TestLandBatch_rebaseGoesThroughRebaser is not parallel: it replaces newBatchRebaser, which the parallel
// tests use.
func TestLandBatch_rebaseGoesThroughRebaser(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	var rebases atomic.Int64
	original := newBatchRebaser
	newBatchRebaser = func(repo *localrepo.Repo) batchRebaser {
		return countingRebaser{batchRebaser: original(repo), rebases: &rebases}
	}
	t.Cleanup(func() { newBatchRebaser = original })

	ctx, cfg, client := setupLandBatchService(t, ctx, batchingGraft())
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(baseEntry))
	first := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
		gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "first", Content: "first"}))
	second := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
		gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "second", Content: "second"}))

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, first))
	require.NoError(t, err)
	require.Equal(t, int64(0), rebases.Load(), "a fast-forward is not rebased")
	require.Equal(t, first.String(), resp.GetLandedCommitId())

	resp, err = client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, second))
	require.NoError(t, err)
	require.Equal(t, int64(1), rebases.Load(), "a diverged change is rebased once, through the rebaser")
	require.True(t, resp.GetRewritten())
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, git.ObjectID(resp.GetLandedCommitId()))
}

// landSequence lands the same changes on a new repository, with the graph ancestry of the configuration
// set to graph and the derived-data store disabled when derivedDisabled is set. It returns the
// outcome of each landing and whether the graph index was stored in the repository.
func landSequence(t *testing.T, ctx context.Context, graph, derivedDisabled bool) ([]string, bool) {
	graft := batchingGraft()
	graft.Land.GraphAncestry = graph
	graft.Derived.Disabled = derivedDisabled

	ctx, cfg, client := setupLandBatchService(t, ctx, graft)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(baseEntry))
	first := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
		gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "first", Content: "first"}))
	second := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
		gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "second", Content: "second"}))
	third := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(first),
		gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "first", Content: "first"},
			gittest.TreeEntry{Mode: "100644", Path: "third", Content: "third"}))

	var outcomes []string
	for _, commit := range []git.ObjectID{first, first, second, third} {
		resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, commit))
		require.NoError(t, err)

		outcomes = append(outcomes, fmt.Sprintf("already=%t rewritten=%t landed=%t",
			resp.GetAlreadyLanded(), resp.GetRewritten(), resp.GetLandedCommitId() == commit.String()))
	}

	_, statErr := os.Stat(filepath.Join(repoPath, "graft", "derived", "graph"))
	return outcomes, statErr == nil
}

func TestLandBatch_graphAncestryMatchesGit(t *testing.T) {
	t.Parallel()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)

	withGit, storedGit := landSequence(t, ctx, false, false)
	withGraph, storedGraph := landSequence(t, ctx, true, false)
	withGraphNoStore, storedNoStore := landSequence(t, ctx, true, true)

	require.Equal(t, withGit, withGraph)
	require.Equal(t, withGit, withGraphNoStore)
	require.False(t, storedGit, "git ancestry must not write the graph index")
	require.True(t, storedGraph, "graph ancestry must write the graph index")
	require.False(t, storedNoStore, "graph ancestry must not write the graph index when the derived store is disabled")
	require.Equal(t, []string{
		"already=false rewritten=false landed=true",
		"already=true rewritten=false landed=true",
		"already=false rewritten=true landed=false",
		"already=false rewritten=true landed=false",
	}, withGit)
}
