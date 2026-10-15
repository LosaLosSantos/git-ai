package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	housekeepingmgr "github.com/feder-cr/open_scion/internal/git/housekeeping/manager"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/service/setup"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/gitaly/transaction"
	"github.com/feder-cr/open_scion/internal/gitlab"
	"github.com/feder-cr/open_scion/internal/graft/derived"
	_ "github.com/feder-cr/open_scion/internal/graft/derived/changedpaths" // registers the changed-paths warmer
	_ "github.com/feder-cr/open_scion/internal/graft/derived/graph"        // registers the graph warmer
	"github.com/feder-cr/open_scion/internal/graft/eager"
	"github.com/feder-cr/open_scion/internal/grpc/backchannel"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/sdk"
)

// The phase-3 end-to-end tests cover batched landing (WP21, WP22), the
// changeset members that record per-change attribution (WP23), eager derived
// data (WP24, WP26) and the indexed path history (WP27). They run against the
// example configuration, or against a configuration of worker agents, like the
// phase-2 tests.

// phase3Agents is the number of agents of the batching and load scenarios.
const phase3Agents = 100

// landBatchOperation is the operation of the changesets of batched lands, see
// internal/graft/changelog.
const landBatchOperation = "land_batch"

// graftBatchActor is the actor of the combined update of a batch. The members
// carry the real actors, see the reconciliation rules of PLAN3.md.
const graftBatchActor = "graft-batch"

// outsiderName is an agent that the policy of protectedGraft does not admit to
// main.
const outsiderName = "outsider"

// landBatchMembers returns the actor of every member of the batched lands,
// keyed by the commit that the member was submitted as. A batch that changed
// the branch for several requests is a land_batch changeset whose actor is
// graft-batch; a batch in which a single request changed the branch is a
// normal land changeset with the request's own actor and that one member
// (doc/graft/PLAN4.md, "BATCH OF ONE KEEPS ITS MEMBER"). A commit that is a
// member twice fails the test.
func landBatchMembers(ctx context.Context, t *testing.T, repo *sdk.Repo) map[string]string {
	t.Helper()

	members := map[string]string{}
	for _, cs := range allChangesets(ctx, t, repo) {
		switch {
		case cs.GetOperation() == landBatchOperation:
			require.Equal(t, graftBatchActor, cs.GetActor(), "the combined update of a land_batch changeset runs as %s", graftBatchActor)
		case cs.GetOperation() == "land" && len(cs.GetMembers()) > 0:
			require.Len(t, cs.GetMembers(), 1, "a land changeset carries at most its own member")
			require.Equal(t, cs.GetMembers()[0].GetActor(), cs.GetActor(), "a batch of one is recorded with its member's actor")
		default:
			continue
		}
		for _, m := range cs.GetMembers() {
			_, dup := members[m.GetOriginalCommitId()]
			require.False(t, dup, "commit %s is a member of two changesets", m.GetOriginalCommitId())
			members[m.GetOriginalCommitId()] = m.GetActor()
		}
	}

	return members
}

// casUpdateRef moves branch from old to next if the branch still points at
// old. It reports whether the update won the compare-and-swap. The lane
// updates the same branch, so an external writer loses some races on purpose.
// A lost race makes git update-ref exit with status 128.
func casUpdateRef(t *testing.T, s gitalyStack, branch, old, next string) bool {
	t.Helper()

	gittest.ExecOpts(t, s.cfg, gittest.ExecConfig{ExpectedExitCode: 128},
		"-C", s.repoPath, "update-ref", "refs/heads/"+branch, next, old)

	return s.git(t, "rev-parse", "refs/heads/"+branch) == next
}

// TestPhase3BatchedLandingAllPrivateChanges lands phase3Agents private changes
// on one branch at the same time, each change with the token of its own
// agent. Every change lands, the lands share batches, and the average number
// of attempts stays at most two. The land_batch changesets hold a member for
// every change, and every member names the agent that submitted it.
func TestPhase3BatchedLandingAllPrivateChanges(t *testing.T) {
	t.Parallel()

	graft, tokens := workerGraft(phase3Agents)
	s := startGitaly(t, graft, sharedTestToken)
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	repos := make([]*sdk.Repo, phase3Agents)
	for i := range repos {
		repos[i] = s.repoAs(t, tokens[workerName(i)])
	}

	const branch = "batch-private"
	_, err := landAs(ctx, repos[0], workerName(0), branch, base)
	require.NoError(t, err)

	commits := make([]string, phase3Agents)
	for i := range commits {
		commits[i], err = sharedChange(ctx, repos[i], workerName(i), base, 1, false)
		require.NoError(t, err)
	}

	results := make([]*sdk.LandResult, phase3Agents)
	tally := runLoad(phase3Agents, func(agent int) error {
		res, err := landOn(ctx, repos[agent], branch, commits[agent], false)
		results[agent] = res
		return err
	})
	require.NoError(t, tally.check(phase3Agents))
	require.Equal(t, loadTally{Landed: phase3Agents}, tally, "every private change lands")

	var attempts int32
	var shared int
	for i, res := range results {
		require.NotNil(t, res, "land %d has a result", i)
		attempts += res.Attempts
		require.Positive(t, res.Batch.Position, "land %d reports its position", i)
		require.LessOrEqual(t, res.Batch.Position, res.Batch.Size, "land %d position within its batch", i)
		if res.Batch.Size > 1 {
			shared++
		}
	}
	require.Positive(t, shared, "concurrent lands share batches")
	require.LessOrEqual(t, attempts, int32(2*phase3Agents), "on average at most two attempts per change")

	members := landBatchMembers(ctx, t, repos[0])
	require.Len(t, members, phase3Agents, "the land_batch members cover every landed change")
	for i, commit := range commits {
		actor, ok := members[commit]
		require.True(t, ok, "change %d is a member of a land_batch changeset", i)
		require.Equal(t, workerName(i), actor, "change %d is attributed to the agent that submitted it", i)
	}

	files := s.git(t, "ls-tree", "-r", "--name-only", branch, "agents")
	require.Len(t, strings.Split(files, "\n"), phase3Agents)
}

// TestPhase3Load100AgentsMixed is the load scenario of WP30. 90 agents land
// private changes and 10 land shared changes that conflict in the same file,
// all on one branch and all at once. The outcome must match
// expectedMixedOutcome whether or not the lands are batched, and the
// land_batch members name exactly the changes that landed.
func TestPhase3Load100AgentsMixed(t *testing.T) {
	t.Parallel()

	const private, shared = 90, 10

	graft, tokens := workerGraft(private + shared)
	s := startGitaly(t, graft, sharedTestToken)
	ctx := s.ctx

	repos := make([]*sdk.Repo, private+shared)
	for i := range repos {
		repos[i] = s.repoAs(t, tokens[workerName(i)])
	}

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	const branch = "load-mixed"
	_, err := landAs(ctx, repos[0], workerName(0), branch, base)
	require.NoError(t, err)

	commits := make([]string, private+shared)
	for i := 0; i < private; i++ {
		commits[i], err = sharedChange(ctx, repos[i], workerName(i), base, 1, false)
		require.NoError(t, err)
	}
	for i := 0; i < shared; i++ {
		agent := private + i
		commits[agent], err = sharedChange(ctx, repos[agent], workerName(agent), base, 1, true)
		require.NoError(t, err)
	}

	tally := runLoad(private+shared, func(agent int) error {
		_, err := landOn(ctx, repos[agent], branch, commits[agent], false)
		return err
	})
	require.NoError(t, tally.check(private+shared))
	require.Equal(t, expectedMixedOutcome(private, shared), tally, "outcome of the mixed load")

	// The private changes and exactly one of the shared changes land.
	files := s.git(t, "ls-tree", "-r", "--name-only", branch, "agents")
	require.Len(t, strings.Split(files, "\n"), private+1)

	members := landBatchMembers(ctx, t, repos[0])
	require.Len(t, members, private+1, "the land_batch members are exactly the landed changes")
	for agent := range commits {
		actor, ok := members[commits[agent]]
		if !ok {
			continue
		}
		require.Equal(t, workerName(agent), actor, "change %d is attributed to the agent that submitted it", agent)
	}
}

// TestPhase3ExternalWriterRacesLane lands private changes while an external
// writer keeps moving the same branch with compare-and-swap updates. Every
// change must land, every commit the writer won with must stay in the history
// of the branch, at least one of the writer's wins must come while the lands
// are still pending, and at least one land must have rebuilt its batch or
// retried after a lost race.
func TestPhase3ExternalWriterRacesLane(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	const branch = "race-external"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)

	const changes = 20
	commits := make([]string, changes)
	for i := range commits {
		commits[i], err = sharedChange(ctx, repo, fmt.Sprintf("agent%d", i), base, 1, false)
		require.NoError(t, err)
	}

	results := make([]*sdk.LandResult, changes)
	var tally loadTally
	landsDone := make(chan struct{})
	go func() {
		defer close(landsDone)
		tally = runLoad(changes, func(agent int) error {
			res, err := landOn(ctx, repo, branch, commits[agent], false)
			results[agent] = res
			return err
		})
	}()

	// The external writer wins a few races. Its wins are small enough for the
	// lane to rebuild a batch at most a few times, far below the limit of ten
	// rebuilds, so every change still lands.
	const externalWins = 5
	const maxWriterAttempts = 1000
	var externals []string
	winsWhilePending := 0
	for attempt := 1; len(externals) < externalWins; attempt++ {
		require.LessOrEqual(t, attempt, maxWriterAttempts, "the external writer never won enough races")

		old := s.git(t, "rev-parse", "refs/heads/"+branch)
		next, err := sharedChange(ctx, repo, "external", old, attempt, false)
		require.NoError(t, err)
		if !casUpdateRef(t, s, branch, old, next) {
			continue
		}

		externals = append(externals, next)
		select {
		case <-landsDone:
		default:
			winsWhilePending++
		}
	}
	require.Positive(t, winsWhilePending, "the external writer won while the lands were still pending")

	<-landsDone
	require.NoError(t, tally.check(changes))
	require.Equal(t, loadTally{Landed: changes}, tally, "every change lands despite the writer")

	var maxAttempts int32
	for i, res := range results {
		require.NotNil(t, res, "land %d has a result", i)
		if res.Attempts > maxAttempts {
			maxAttempts = res.Attempts
		}
	}
	require.Greater(t, maxAttempts, int32(1), "a land rebuilt its batch or retried after a lost race")

	// Exit status 0 means each external commit is an ancestor of the branch.
	for _, external := range externals {
		s.git(t, "merge-base", "--is-ancestor", external, branch)
	}
}

// hookFallbackPrefix is the prefix of the branches that forbiddenHookScript
// guards.
const hookFallbackPrefix = "hook-fallback-"

// forbiddenHookScript returns a pre-receive hook that rejects an update of a
// branch named hookFallbackPrefix* when the new tip holds forbidden.txt at its
// root. For every update of such a branch it appends one line to logPath:
// "<ref> <accept|reject> <commits>", where commits is the number of commits the
// update brings. A test can thus tell a rejected batch from a rejected single
// land.
func forbiddenHookScript(logPath string) string {
	return `#!/bin/sh
while read old new ref; do
	case "$ref" in
	refs/heads/` + hookFallbackPrefix + `*)
		count=$(git rev-list --count "$old..$new")
		if git ls-tree -r --name-only "$new" | grep -qx forbidden.txt; then
			echo "$ref reject $count" >> '` + logPath + `'
			echo "forbidden.txt may not land on $ref" >&2
			exit 1
		fi
		echo "$ref accept $count" >> '` + logPath + `'
		;;
	esac
done
exit 0
`
}

// hookDecision is one line of the log of forbiddenHookScript.
type hookDecision struct {
	ref      string
	rejected bool
	commits  int
}

// readHookDecisions parses the log of forbiddenHookScript.
func readHookDecisions(t *testing.T, logPath string) []hookDecision {
	t.Helper()

	logged, err := os.ReadFile(logPath)
	require.NoError(t, err, "the hook saw the updates of the branches")

	var decisions []hookDecision
	for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		var d hookDecision
		var verdict string
		_, err := fmt.Sscan(line, &d.ref, &verdict, &d.commits)
		require.NoError(t, err, "hook log line %q", line)
		require.Contains(t, []string{"accept", "reject"}, verdict, "hook log line %q", line)
		d.rejected = verdict == "reject"
		decisions = append(decisions, d)
	}

	return decisions
}

// TestPhase3HookRejectionFallsBackUnbatched checks that when a hook rejects
// the combined update of a batch, the requests of the batch are retried
// alone, so the rejection is attributed to the request that caused it and the
// others still land. The rejecting change adds forbidden.txt, which the hook
// refuses on the branch.
//
// The hook log names the decision of every update, so the test requires that
// an update of several commits was REJECTED: a batch of several requests that
// the hook accepted, followed by forbidden.txt alone, would not exercise the
// fallback. Whether forbidden.txt shares a batch depends on when the requests
// arrive within the batch window, so the scenario is repeated on fresh
// branches until a round rejects a batch. Every round must still land every
// other change.
func TestPhase3HookRejectionFallsBackUnbatched(t *testing.T) {
	t.Parallel()

	ex := loadExample(t)
	ex.graft.Land.Batch.Window = "300ms"
	s := startGitaly(t, ex.graft, ex.sharedToken)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})

	// The branches exist before the hook does, so the hook only sees the lands
	// of the changes.
	const rounds = 5
	for round := 0; round < rounds; round++ {
		_, err := landAs(ctx, repo, "planner", fmt.Sprintf("%s%d", hookFallbackPrefix, round), base)
		require.NoError(t, err)
	}

	updatesLog := filepath.Join(t.TempDir(), "updates.log")
	gittest.WriteCustomHook(t, s.repoPath, "pre-receive", []byte(forbiddenHookScript(updatesLog)))

	const changes = 10
	const forbidden = changes - 1
	for round := 0; round < rounds; round++ {
		branch := fmt.Sprintf("%s%d", hookFallbackPrefix, round)
		ref := "refs/heads/" + branch

		commits := make([]string, changes)
		for i := range commits {
			path := fmt.Sprintf("hooked/%d-%02d.txt", round, i)
			if i == forbidden {
				path = "forbidden.txt"
			}
			var err error
			commits[i], err = commitFile(ctx, repo, "planner", fmt.Sprintf("topic/hook-%d-%02d", round, i), base, path, "content\n")
			require.NoError(t, err)
		}

		results := make([]*sdk.LandResult, changes)
		errs := make([]error, changes)
		var wg sync.WaitGroup
		for i := range commits {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = landOn(ctx, repo, branch, commits[i], false)
			}(i)
		}
		wg.Wait()

		aloneAfterFallback := 0
		for i, err := range errs {
			if i == forbidden {
				require.Error(t, err, "the change the hook rejects does not land")
				require.True(t, sdk.IsRejectedByHook(err), "the rejection is attributed to the change that caused it: %v", err)
				require.ErrorContains(t, err, "forbidden.txt may not land")
				continue
			}
			require.NoError(t, err, "change %d lands although a batch it was in was rejected", i)
			if results[i].Batch.Size == 1 {
				aloneAfterFallback++
			}
		}

		files := s.git(t, "ls-tree", "-r", "--name-only", branch)
		require.NotContains(t, strings.Split(files, "\n"), "forbidden.txt", "the rejected change must not land")
		hooked := strings.Split(s.git(t, "ls-tree", "-r", "--name-only", branch, "hooked"), "\n")
		require.Len(t, hooked, changes-1, "every other change lands")

		// The largest update of this branch that the hook rejected. Updates of
		// one commit are the rejected forbidden.txt landing alone.
		rejectedBatch, acceptedAlone := 0, 0
		for _, d := range readHookDecisions(t, updatesLog) {
			switch {
			case d.ref != ref:
			case d.rejected:
				rejectedBatch = max(rejectedBatch, d.commits)
			case d.commits == 1:
				acceptedAlone++
			}
		}
		if rejectedBatch <= 1 {
			t.Logf("round %d: forbidden.txt did not share a batch, repeating on a fresh branch", round)
			continue
		}

		// Every other request of the rejected batch landed alone through the
		// fallback: the hook accepted it as an update of one commit, and its
		// response reports a batch of one.
		require.GreaterOrEqual(t, acceptedAlone, rejectedBatch-1, "the other requests of the rejected batch are landed one by one")
		require.GreaterOrEqual(t, aloneAfterFallback, rejectedBatch-1, "the requests landed by the fallback report a batch of one")
		return
	}

	require.Fail(t, "no round rejected a batch of several requests, so the fallback was never exercised")
}

// TestPhase3CancelledRequestsAreDropped checks a request that is cancelled
// while it waits in the batch window. The request reaches the server, its
// deadline passes before the window closes, and it returns at once without
// landing and without being a member of any batch. A live request submitted
// with it lands alone, so the cancelled request does not hold it back.
func TestPhase3CancelledRequestsAreDropped(t *testing.T) {
	t.Parallel()

	ex := loadExample(t)
	ex.graft.Land.Batch.Window = "1500ms"
	s := startGitaly(t, ex.graft, ex.sharedToken)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	const branch = "cancelled"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)

	live, err := commitFile(ctx, repo, "planner", "topic/cancel-live", base, "cancel/live.txt", "content\n")
	require.NoError(t, err)
	dropped, err := commitFile(ctx, repo, "planner", "topic/cancel-dropped", base, "cancel/dropped.txt", "content\n")
	require.NoError(t, err)

	var liveRes *sdk.LandResult
	var liveErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		liveRes, liveErr = landOn(ctx, repo, branch, live, false)
	}()

	doomed, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = landOn(doomed, repo, branch, dropped, false)
	elapsed := time.Since(start)
	wg.Wait()

	require.Contains(t, []codes.Code{codes.Canceled, codes.DeadlineExceeded}, sdk.Code(err), "the cancelled request fails, got %v", err)
	require.Less(t, elapsed, time.Second, "the cancelled request returns before the batch window closes")

	require.NoError(t, liveErr, "the live request lands")
	require.Equal(t, int32(1), liveRes.Batch.Size, "the live request is alone in its batch")

	files := strings.Split(s.git(t, "ls-tree", "-r", "--name-only", branch, "cancel"), "\n")
	require.Contains(t, files, "cancel/live.txt", "the live change lands")
	require.NotContains(t, files, "cancel/dropped.txt", "the cancelled change must not land")

	members := landBatchMembers(ctx, t, repo)
	require.Contains(t, members, live, "the live change is a member")
	require.NotContains(t, members, dropped, "the cancelled change is in no batch")
}

// protectedGraft returns the worker configuration of workerGraft plus an
// outsider agent. Only the workers may update main, so the outsider is denied
// there.
func protectedGraft(n int) (config.Graft, map[string]string) {
	graft, tokens := workerGraft(n)

	token := outsiderName + "-secret-for-tests"
	tokens[outsiderName] = token
	graft.Agents = append(graft.Agents, config.GraftAgent{Name: outsiderName, Token: token})

	return graft, tokens
}

// TestPhase3ProtectedBranchDeniesOutsider lands changes on main, which the
// policy protects against every actor but the workers. The workers all land,
// and the outsider's land on the same branch at the same time is denied with
// PermissionDenied, without holding back the workers.
func TestPhase3ProtectedBranchDeniesOutsider(t *testing.T) {
	t.Parallel()

	const workers = 10
	const agents = workers + 1

	graft, tokens := protectedGraft(workers)
	s := startGitaly(t, graft, sharedTestToken)
	ctx := s.ctx

	names := make([]string, agents)
	for i := range names {
		names[i] = workerName(i)
	}
	names[workers] = outsiderName

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	repos := make([]*sdk.Repo, agents)
	commits := make([]string, agents)
	for i, name := range names {
		repos[i] = s.repoAs(t, tokens[name])
		var err error
		commits[i], err = commitFile(ctx, repos[i], name, "topic/protected-"+name, base,
			fmt.Sprintf("protected/%s.txt", name), "content\n")
		require.NoError(t, err)
	}

	tally := runLoad(agents, func(agent int) error {
		_, err := landOn(ctx, repos[agent], "main", commits[agent], false)
		return err
	})
	require.NoError(t, tally.check(agents))
	require.Equal(t, loadTally{Landed: workers, Denied: 1}, tally, "the workers land on main and the outsider is denied")

	files := strings.Split(s.git(t, "ls-tree", "-r", "--name-only", "main", "protected"), "\n")
	require.Len(t, files, workers, "every worker change is on main")
	require.NotContains(t, files, "protected/"+outsiderName+".txt", "the outsider's change is not on main")
}

// TestPhase3ProtectedBranchBatches lands changes on main, which the policy
// protects against every actor but the workers, all at once. The changes share
// batches and every member names the worker that submitted it. The pre-receive
// hook admits the combined graft-batch update of a land_batch with members, so
// the batch is not split on a protected branch.
func TestPhase3ProtectedBranchBatches(t *testing.T) {
	t.Parallel()

	const workers = 20
	graft, tokens := workerGraft(workers)
	graft.Land.Batch.Window = "300ms"
	s := startGitaly(t, graft, sharedTestToken)
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	repos := make([]*sdk.Repo, workers)
	commits := make([]string, workers)
	for i := range repos {
		repos[i] = s.repoAs(t, tokens[workerName(i)])
		var err error
		commits[i], err = commitFile(ctx, repos[i], workerName(i), "topic/protected-batch-"+workerName(i), base,
			fmt.Sprintf("protected-batch/%02d.txt", i), "content\n")
		require.NoError(t, err)
	}

	results := make([]*sdk.LandResult, workers)
	tally := runLoad(workers, func(agent int) error {
		res, err := landOn(ctx, repos[agent], "main", commits[agent], false)
		results[agent] = res
		return err
	})
	require.NoError(t, tally.check(workers))
	require.Equal(t, loadTally{Landed: workers}, tally, "every worker change lands on main")

	var shared int
	for i, res := range results {
		require.NotNil(t, res, "land %d has a result", i)
		if res.Batch.Size > 1 {
			shared++
		}
	}
	require.Positive(t, shared, "concurrent lands on a protected branch share batches")

	members := landBatchMembers(ctx, t, repos[0])
	require.Len(t, members, workers, "the land_batch members cover every landed change")
	for i, commit := range commits {
		require.Equal(t, workerName(i), members[commit], "change %d is attributed to the worker that submitted it", i)
	}
}

// startEagerGitaly starts a standalone Gitaly like startGitaly, with the eager
// observer of internal/graft/eager wired into the hook manager and a derived
// data pool, as serve.go does for the production server.
func startEagerGitaly(t *testing.T, graft config.Graft, sharedToken string) gitalyStack {
	t.Helper()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	cfg.Auth.Token = sharedToken
	cfg.Graft = graft
	cfg.Graft.Standalone = true

	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	logger := testhelper.SharedLogger(t)
	locator := config.NewLocator(cfg)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)
	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)

	build := func(repo storage.Repository) *localrepo.Repo {
		return localrepo.New(logger, locator, gitCmdFactory, catfileCache, repo)
	}

	pool := derived.NewPool(logger, cfg.Graft.Derived.WorkersOrDefault(), cfg.Graft.Derived.QueueSizeOrDefault(),
		eager.WarmEnvOpener(build, logger, catfileCache))
	poolCtx, cancelPool := context.WithCancel(testhelper.Context(t))
	t.Cleanup(cancelPool)
	go pool.Run(poolCtx)
	t.Cleanup(pool.Close)

	gitlabClient := gitlab.NewStandaloneClient(cfg.Graft, logger)
	backchannelRegistry := backchannel.NewRegistry()
	txMgr := transaction.NewManager(cfg, logger, backchannelRegistry)
	housekeepingManager := housekeepingmgr.New(cfg.Prometheus, logger, txMgr, nil)

	observer := eager.NewObserver(testhelper.Context(t), logger,
		eager.Options{Eager: true, CommitGraph: true},
		eager.Deps{
			Enqueuer:         pool,
			ListNewCommits:   eager.NewCommitsLister(build),
			WriteCommitGraph: eager.CommitGraphWriterFor(build, housekeepingManager),
		},
	)
	t.Cleanup(observer.Close)

	hookMgr := hook.NewManager(
		cfg, locator, logger, gitCmdFactory, nil, txMgr, gitlabClient,
		hook.NewPostReceiveRegistry(),
		hook.WithEagerObserver(observer),
	)

	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
		testserver.WithGitLabClient(gitlabClient),
		testserver.WithBackchannelRegistry(backchannelRegistry),
		testserver.WithTransactionManager(txMgr),
		testserver.WithHookManager(hookMgr),
		testserver.WithHousekeepingManager(housekeepingManager),
	)
	cfg.SocketPath = addr

	// Only the setup call carries gitaly-servers metadata, see startGitaly.
	setupCtx := testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))
	repoProto, repoPath := gittest.CreateRepository(t, setupCtx, cfg)

	return gitalyStack{
		ctx:      ctx,
		cfg:      cfg,
		addr:     addr,
		storage:  cfg.Storages[0].Name,
		relPath:  repoProto.GetRelativePath(),
		repoPath: repoPath,
	}
}

// TestPhase3EagerDerivedDataVisibleAfterLand checks that after a land returns,
// the eager derived data of the landed commit is stored: the commit-graph
// chain is written, and the graph and changed-paths entries of the commit are
// present without any query having computed them.
func TestPhase3EagerDerivedDataVisibleAfterLand(t *testing.T) {
	t.Parallel()

	ex := loadExample(t)
	ex.graft.Derived.Eager = true
	s := startEagerGitaly(t, ex.graft, ex.sharedToken)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	const branch = "eager"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)

	commit, err := commitFile(ctx, repo, "planner", "topic/eager", base, "eager/landed.txt", "eager\n")
	require.NoError(t, err)
	res, err := landOn(ctx, repo, branch, commit, false)
	require.NoError(t, err)
	landed := git.ObjectID(res.LandedCommitID)

	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(s.repoPath, "objects", "info", "commit-graphs", "commit-graph-chain"))
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "the land writes the commit-graph chain")

	store := derived.NewStore(s.repoPath)
	require.Eventually(t, func() bool {
		_, ok, err := store.Kind("graph", 1).Get(landed)
		return err == nil && ok
	}, 10*time.Second, 10*time.Millisecond, "the graph entry of the landed commit is warmed")

	require.Eventually(t, func() bool {
		value, ok, err := store.Kind("changed-paths", 1).Get(landed)
		return err == nil && ok && strings.Contains(string(value), "eager/landed.txt")
	}, 10*time.Second, 10*time.Millisecond, "the changed-paths entry of the landed commit is warmed")
}

// derivedPathHistory returns the IDs of the path history of path from rev,
// computed by the non-indexed (*derived.Deriver).PathHistory without caching.
func derivedPathHistory(t *testing.T, s gitalyStack, rev, path string, limit int) []string {
	t.Helper()

	ctx := testhelper.Context(t)
	logger := testhelper.SharedLogger(t)
	catfileCache := catfile.NewCache(s.cfg)
	t.Cleanup(catfileCache.Stop)

	repo := localrepo.New(logger, config.NewLocator(s.cfg), gittest.NewCommandFactory(t, s.cfg), catfileCache,
		&gitalypb.Repository{StorageName: s.storage, RelativePath: s.relPath})
	reader, release, err := catfileCache.ObjectReader(ctx, repo)
	require.NoError(t, err)
	t.Cleanup(release)

	deriver, err := derived.NewDeriver(ctx, logger, repo, reader, false)
	require.NoError(t, err)

	ids := []string{}
	require.NoError(t, deriver.PathHistory(ctx, git.ObjectID(s.git(t, "rev-parse", rev)), path, limit,
		func(commit *catfile.Commit) error {
			ids = append(ids, commit.GetId())
			return nil
		}))

	return ids
}

// TestPhase3IndexedPathHistoryMatchesDeriver checks that the path history the
// DerivedDataService returns, which reads the changed-paths index, is the same
// as the non-indexed history of the deriver on the final history. The history
// has a merge, a deletion and a re-add of a file, and a directory.
func TestPhase3IndexedPathHistoryMatchesDeriver(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx
	planner := sdk.User{ID: "planner", Name: "planner", Email: "planner@example.com"}

	base := writeMain(t, s, map[string]string{"README.md": "history\n"})
	const branch = "history"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)

	commitOn := func(name, start string, actions ...sdk.Action) string {
		t.Helper()

		res, err := repo.CommitFiles(ctx, sdk.CommitOptions{
			Branch:   "topic/" + name,
			StartRev: start,
			Message:  "history " + name,
			Author:   sdk.Signature{Name: "planner", Email: "planner@example.com"},
			User:     planner,
			Actions:  actions,
		})
		require.NoError(t, err)

		return res.CommitID
	}

	tip := base
	step := 0
	advance := func(actions ...sdk.Action) string {
		t.Helper()

		step++
		commit := commitOn(fmt.Sprintf("history-%02d", step), tip, actions...)
		res, err := landOn(ctx, repo, branch, commit, false)
		require.NoError(t, err)
		tip = res.LandedCommitID

		return tip
	}

	advance(sdk.Put{Path: "docs/a.txt", Content: []byte("a1\n")})
	advance(sdk.Put{Path: "README.md", Content: []byte("r2\n"), Overwrite: true})
	side := commitOn("side", advance(sdk.Put{Path: "docs/b/c.txt", Content: []byte("c1\n")}),
		sdk.Put{Path: "side.txt", Content: []byte("side\n")})
	advance(sdk.Delete{Path: "docs/a.txt"})
	advance(sdk.Put{Path: "docs/a.txt", Content: []byte("a2\n")})
	advance(sdk.Put{Path: "other.txt", Content: []byte("other\n")})
	advance(sdk.Put{Path: "docs/b/c.txt", Content: []byte("c2\n"), Overwrite: true})

	_, err = repo.Land(ctx, sdk.LandOptions{
		Branch:   branch,
		Commit:   side,
		User:     planner,
		Strategy: sdk.LandMerge,
	})
	require.NoError(t, err)
	require.NotEmpty(t, s.git(t, "rev-list", "--merges", branch), "the history has a merge commit")

	for _, path := range []string{"README.md", "docs/a.txt", "docs", "docs/b/c.txt", "other.txt", "side.txt", "missing.txt"} {
		t.Run(path, func(t *testing.T) {
			want := derivedPathHistory(t, s, "refs/heads/"+branch, path, 100)

			got, err := repo.PathHistory(ctx, branch, path, 100)
			require.NoError(t, err)
			ids := make([]string, 0, len(got))
			for _, c := range got {
				ids = append(ids, c.GetId())
			}

			require.Equal(t, want, ids, "the indexed history equals the deriver's history")
		})
	}
}

// TestPhase3SingleLandIsABatch checks that a land that waits alone reports a
// batch of one and is recorded as a normal land changeset, with the agent as
// actor and its single member (doc/graft/PLAN4.md, "BATCH OF ONE KEEPS ITS
// MEMBER").
func TestPhase3SingleLandIsABatch(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{"README.md": "single\n"})
	const branch = "single-batch"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)

	commit, err := commitFile(ctx, repo, "planner", "topic/single", base, "single.txt", "one\n")
	require.NoError(t, err)
	res, err := landOn(ctx, repo, branch, commit, false)
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Batch.Size)
	require.Equal(t, int32(1), res.Batch.Position)

	members := landBatchMembers(ctx, t, repo)
	require.Equal(t, "planner", members[commit])
	require.Len(t, members, 1)

	var single *sdk.Changeset
	for _, cs := range allChangesets(ctx, t, repo) {
		if string(cs.GetReference()) == "refs/heads/"+branch && len(cs.GetMembers()) == 1 {
			single = cs
		}
	}
	require.NotNil(t, single, "the single land is in the changeset log")
	require.Equal(t, "land", single.GetOperation())
	require.Equal(t, "planner", single.GetActor())
}
