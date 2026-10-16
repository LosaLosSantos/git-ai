package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/landlane"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/sdk"
)

// The phase-4 end-to-end tests (WP39) cover the batch latency work: the graph
// ancestry checks must not change the outcome of a batch, the adaptive batch
// sizing and early completion of the executor, and the plain land of a batch
// of one. The tests run against the real executor of WP35 and the lanes of
// WP37.

// phase4GaugeName is the gauge of the effective maximum batch size, see
// internal/graft/landlane.
const phase4GaugeName = "graft_land_lane_effective_max_size"

// phase4NextEffectiveMax is the model of the adaptive batch sizing of WP37
// that the landlane gauge graft_land_lane_effective_max_size must follow.
// After a batch that took longer than the target the effective size halves,
// never below one. After a batch that took less than half the target it
// grows by a quarter, at least one, never above the configured maximum.
// Otherwise it stays. A target of zero turns the adaptation off.
func phase4NextEffectiveMax(current, maxSize int, duration, target time.Duration) int {
	switch {
	case target <= 0:
		return current
	case duration > target:
		return max(1, current/2)
	case duration < target/2:
		return min(maxSize, current+max(1, current/4))
	default:
		return current
	}
}

// TestPhase4AdaptiveSizingModel checks the model of the adaptive sizing that
// TestPhase4AdaptiveSizingVisible compares the gauge with. It needs no server.
func TestPhase4AdaptiveSizingModel(t *testing.T) {
	t.Parallel()

	const (
		maxSize = 8
		target  = 500 * time.Millisecond
		slow    = 2 * target
		fast    = target / 4
		steady  = target * 3 / 4
	)

	steps := []struct {
		desc     string
		duration time.Duration
		want     int
	}{
		{"slow halves", slow, 4},
		{"slow halves again", slow, 2},
		{"slow to the floor", slow, 1},
		{"slow stays at one", slow, 1},
		{"fast grows by one", fast, 2},
		{"fast grows by one again", fast, 3},
		{"fast grows by one more", fast, 4},
		{"steady keeps the size", steady, 4},
		{"fast grows to five", fast, 5},
		{"fast grows to six", fast, 6},
		{"fast grows to seven", fast, 7},
		{"fast grows to the maximum", fast, 8},
		{"fast stays at the maximum", fast, 8},
	}

	current := maxSize
	for _, step := range steps {
		current = phase4NextEffectiveMax(current, maxSize, step.duration, target)
		require.Equal(t, step.want, current, step.desc)
	}

	require.Equal(t, 5, phase4NextEffectiveMax(5, maxSize, slow, 0), "a zero target turns the adaptation off")
}

// phase4MixedRun lands private disjoint changes and shared changes, all made
// from one base and all touching the shared log, on main at the same time,
// with workerGraft tokens. It returns the tally of the lands and the files
// that the agents added to main.
func phase4MixedRun(t *testing.T, graft config.Graft, tokens map[string]string, private, shared int) (loadTally, []string) {
	t.Helper()

	s := startGitaly(t, graft, sharedTestToken)
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	agents := private + shared
	repos := make([]*sdk.Repo, agents)
	commits := make([]string, agents)
	for i := range repos {
		name := workerName(i)
		repos[i] = s.repoAs(t, tokens[name])
		var err error
		commits[i], err = sharedChange(ctx, repos[i], name, base, 1, i >= private)
		require.NoError(t, err)
	}

	tally := runLoad(agents, func(agent int) error {
		_, err := landOn(ctx, repos[agent], "main", commits[agent], false)
		return err
	})
	require.NoError(t, tally.check(agents))

	files := strings.Split(s.git(t, "ls-tree", "-r", "--name-only", "main", "agents"), "\n")
	return tally, files
}

// TestPhase4GraphAncestryMatchesGit lands the same mixed batch with the graph
// ancestry checks on and off. The tallies must be equal and equal to the
// expected outcome, and the same private changes must be on main.
func TestPhase4GraphAncestryMatchesGit(t *testing.T) {
	t.Parallel()

	const private, shared = 17, 3
	var tallies [2]loadTally
	var files [2][]string
	for i, on := range []bool{false, true} {
		graft, tokens := workerGraft(private + shared)
		graft.Land.Batch.Window = "300ms"
		graft.Land.GraphAncestry = on

		tallies[i], files[i] = phase4MixedRun(t, graft, tokens, private, shared)
	}

	want := expectedMixedOutcome(private, shared)
	require.Equal(t, want, tallies[0], "the outcome without graph ancestry")
	require.Equal(t, want, tallies[1], "the outcome with graph ancestry")

	for run, list := range files {
		var privateOnMain, sharedOnMain int
		for i := 0; i < private+shared; i++ {
			if !slices.Contains(list, fmt.Sprintf("agents/%s/1.txt", workerName(i))) {
				continue
			}
			if i < private {
				privateOnMain++
			} else {
				sharedOnMain++
			}
		}
		require.Equal(t, private, privateOnMain, "run %d: every private change is on main", run)
		require.Equal(t, 1, sharedOnMain, "run %d: exactly one shared change is on main", run)
	}
}

// TestPhase4BatchOfOneIsLand checks that a change that lands alone in a batch
// is recorded as an ordinary land by the real actor. The land keeps its single
// member, so the original commit maps to the commit it landed as, and the
// policy check of the land is the one of an unbatched land.
func TestPhase4BatchOfOneIsLand(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	repo := s.repoAs(t, exampleTokens["planner"])
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{"README.md": "single\n"})
	const branch = "phase4-single"
	_, err := landAs(ctx, repo, "planner", branch, base)
	require.NoError(t, err)

	commit, err := commitFile(ctx, repo, "planner", "topic/phase4-single", base, "phase4-single.txt", "one\n")
	require.NoError(t, err)
	res, err := landOn(ctx, repo, branch, commit, false)
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Batch.Size, "the change is a batch of one")

	var landed []*sdk.Changeset
	for _, cs := range allChangesets(ctx, t, repo) {
		if string(cs.GetReference()) == "refs/heads/"+branch && len(cs.GetMembers()) == 1 {
			landed = append(landed, cs)
		}
	}
	require.Len(t, landed, 1, "exactly one changeset records the batch of one")

	cs := landed[0]
	require.Equal(t, "land", cs.GetOperation(), "a batch of one is a land")
	require.Equal(t, "planner", cs.GetActor(), "the actor is the real land actor")

	member := cs.GetMembers()[0]
	require.Equal(t, commit, member.GetOriginalCommitId(), "the member is the submitted commit")
	require.Equal(t, res.LandedCommitID, member.GetCommitId(), "the member is the landed commit")
	require.Equal(t, "planner", member.GetActor(), "the member is attributed to its agent")
}

// startGitalyWithLanes starts a standalone Gitaly like startGitaly, except that
// its land batches run in lanes that the test owns, so that the test can read
// the metrics of the lanes. The lanes are closed when the test ends.
func startGitalyWithLanes(t *testing.T, graft config.Graft, lanes *landlane.Shared) gitalyStack {
	t.Helper()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	cfg.Auth.Token = sharedTestToken
	cfg.Graft = graft
	cfg.Graft.Standalone = true

	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	registrar := func(srv *grpc.Server, deps *service.Dependencies) {
		deps.GraftLandLanes = lanes
		setup.RegisterAll(srv, deps)
	}
	addr := testserver.RunGitalyServer(t, cfg, registrar,
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))),
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

// newTestLanes builds the lanes of a configuration, as the server does, and
// closes them when the test ends.
func newTestLanes(t *testing.T, graft config.Graft) *landlane.Shared {
	t.Helper()

	cfg, err := landlane.ConfigFromGraft(graft.Land.Batch)
	require.NoError(t, err)

	lanes := landlane.NewShared(cfg)
	t.Cleanup(lanes.Close)

	return lanes
}

// effectiveMaxSize reads the gauge of the effective maximum batch size of the
// lanes, summed over their lanes.
func effectiveMaxSize(t *testing.T, lanes *landlane.Shared) float64 {
	t.Helper()

	registry := prometheus.NewRegistry()
	require.NoError(t, registry.Register(lanes))

	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == phase4GaugeName {
			require.Len(t, family.GetMetric(), 1)
			return family.GetMetric()[0].GetGauge().GetValue()
		}
	}

	require.FailNow(t, "the gauge is not exported", "missing %s", phase4GaugeName)
	return 0
}

// slowUpdateHookScript returns a pre-receive hook that sleeps for delay before it
// accepts an update, for as long as the marker file exists. Creating and
// removing the marker switches the updates between slow and fast.
func slowUpdateHookScript(marker string, delay time.Duration) string {
	return fmt.Sprintf(`#!/bin/sh
while read old new ref; do :; done
if [ -f '%s' ]; then
	sleep %.1f
fi
exit 0
`, marker, delay.Seconds())
}

// TestPhase4AdaptiveSizingVisible checks that the effective maximum size gauge
// shrinks when batch updates are slower than the target latency, and grows
// back when they are fast. The bounds hold however the rounds split into
// batches: every slow batch halves the size and every fast batch grows it.
func TestPhase4AdaptiveSizingVisible(t *testing.T) {
	t.Parallel()

	const (
		agents   = 4
		maxSize  = 8
		target   = 2 * time.Second
		slowHook = 3 * time.Second
	)

	graft, tokens := workerGraft(agents)
	graft.Land.Batch.Window = "100ms"
	graft.Land.Batch.MaxSize = maxSize
	graft.Land.Batch.TargetLatency = target.String()
	lanes := newTestLanes(t, graft)
	s := startGitalyWithLanes(t, graft, lanes)
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	repos := make([]*sdk.Repo, agents)
	for i := range repos {
		repos[i] = s.repoAs(t, tokens[workerName(i)])
	}

	const branch = "adaptive"
	_, err := landAs(ctx, repos[0], workerName(0), branch, base)
	require.NoError(t, err)

	marker := filepath.Join(t.TempDir(), "slow")
	gittest.WriteCustomHook(t, s.repoPath, "pre-receive", []byte(slowUpdateHookScript(marker, slowHook)))

	round := 0
	landRound := func() {
		t.Helper()

		round++
		commits := make([]string, agents)
		for i := range commits {
			var err error
			commits[i], err = commitFile(ctx, repos[i], workerName(i), fmt.Sprintf("topic/adaptive-%d-%d", round, i), base,
				fmt.Sprintf("adaptive/%d/%02d.txt", round, i), "content\n")
			require.NoError(t, err)
		}

		tally := runLoad(agents, func(agent int) error {
			_, err := landOn(ctx, repos[agent], branch, commits[agent], false)
			return err
		})
		require.Equal(t, loadTally{Landed: agents}, tally, "every change of round %d lands", round)
	}

	// Two rounds of slow batches. Each round has at least one batch, and every
	// batch is slower than the target, so the size halves at least twice.
	require.NoError(t, os.WriteFile(marker, nil, 0o600))
	landRound()
	landRound()

	slow := effectiveMaxSize(t, lanes)
	require.LessOrEqual(t, slow, float64(maxSize/4), "two rounds of slow batches halve the size at least twice")
	require.GreaterOrEqual(t, slow, float64(1), "the size never drops below one")

	// Three rounds of fast batches. Each batch grows the size by at least one.
	require.NoError(t, os.Remove(marker))
	landRound()
	landRound()
	landRound()

	grown := effectiveMaxSize(t, lanes)
	require.Greater(t, grown, slow, "fast batches grow the effective size")
	require.LessOrEqual(t, grown, float64(maxSize), "the size never exceeds the maximum")
}

// TestPhase4EarlyCompletionPolicyDenied checks that a policy-denied request
// which shares a batch with landing members returns before the slow update of
// that batch, while the members wait for the update. The denial is final before
// the update, so it does not wait for it.
func TestPhase4EarlyCompletionPolicyDenied(t *testing.T) {
	t.Parallel()

	const (
		workers  = 10
		slowHook = 3 * time.Second
	)

	graft, tokens := protectedGraft(workers)
	graft.Land.Batch.Window = "300ms"
	s := startGitaly(t, graft, sharedTestToken)
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	marker := filepath.Join(t.TempDir(), "slow")
	gittest.WriteCustomHook(t, s.repoPath, "pre-receive", []byte(slowUpdateHookScript(marker, slowHook)))
	require.NoError(t, os.WriteFile(marker, nil, 0o600))

	// The workers and the outsider, which the policy denies on main, all land
	// at once, so that they share the batch window.
	names := make([]string, 0, workers+1)
	for i := 0; i < workers; i++ {
		names = append(names, workerName(i))
	}
	names = append(names, outsiderName)

	repos := make([]*sdk.Repo, len(names))
	commits := make([]string, len(names))
	for i, name := range names {
		repos[i] = s.repoAs(t, tokens[name])
		var err error
		commits[i], err = commitFile(ctx, repos[i], name, "topic/early-"+name, base,
			fmt.Sprintf("early/%s.txt", name), "content\n")
		require.NoError(t, err)
	}

	type outcome struct {
		res     *sdk.LandResult
		elapsed time.Duration
	}
	outcomes := make([]outcome, len(names))
	tally := runLoad(len(names), func(agent int) error {
		start := time.Now()
		res, err := landOn(ctx, repos[agent], "main", commits[agent], false)
		outcomes[agent] = outcome{res: res, elapsed: time.Since(start)}
		return err
	})
	require.Equal(t, loadTally{Landed: workers, Denied: 1}, tally, "the workers land and the outsider is denied")

	denied := outcomes[workers]
	require.Less(t, denied.elapsed, slowHook/2, "the denied request returns before the slow update of its batch")

	// The batch size counts every request of the batch, the denied one included,
	// so the workers of a batch of size s that holds the denial are s-1 landed
	// members. Their count n then satisfies n+1 = 0 mod s, which a batch of
	// landed members alone never does for s > 1.
	var shared int
	sizes := map[int32]int{}
	for i := 0; i < workers; i++ {
		require.NotNil(t, outcomes[i].res, "worker %d landed", i)
		require.GreaterOrEqual(t, outcomes[i].elapsed, slowHook, "worker %d waits for the batch update", i)
		if size := outcomes[i].res.Batch.Size; size > 1 {
			shared++
			sizes[size]++
		}
	}
	require.Positive(t, shared, "the workers share batches")

	var withDenial bool
	for size, n := range sizes {
		if (n+1)%int(size) == 0 {
			withDenial = true
		}
	}
	require.True(t, withDenial, "the denied request is in a batch with the landing workers, sizes %v", sizes)

	files := strings.Split(s.git(t, "ls-tree", "-r", "--name-only", "main", "early"), "\n")
	require.Len(t, files, workers, "every worker change is on main")
	require.NotContains(t, files, "early/"+outsiderName+".txt", "the denied change is not on main")
}
