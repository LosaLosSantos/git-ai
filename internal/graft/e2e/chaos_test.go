package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// The chaos scenario of phase 4 (WP39) lands many changes at once on two
// protected branches and one open branch. An external writer moves the open
// branch with UserUpdateBranch while the lands run, and some requests are
// cancelled, before the call or in flight. Whatever the interleaving, the
// outcome must satisfy the invariants of checkChaosInvariants. The invariant
// checks are pure functions of a view of the repository, so they are tested
// with fakes in TestChaosInvariantCheckerDetectsViolations.
//
// The plan of the scenario (who lands what, on which branch, which requests are
// cancelled) is drawn from a seed. A failing run prints the seed, and
// GRAFT_CHAOS_SEED replays that plan. The interleaving of the server is not
// replayed by the seed.

const (
	// chaosAgents is the number of agents of the scenario. The last one is an
	// outsider that the policy admits to open branches only.
	chaosAgents = 20
	// chaosSeed is the default seed of the plan of the scenario.
	chaosSeed = 1
	// chaosSeedEnv names the environment variable that overrides chaosSeed.
	chaosSeedEnv = "GRAFT_CHAOS_SEED"
	// chaosBudget bounds the time the lands and the external writer run.
	chaosBudget = 60 * time.Second
	// chaosOutsider is the agent that the protected branches deny.
	chaosOutsider = "outsider-chaos"
	// chaosExternalBranch is the branch the external writer moves.
	chaosExternalBranch = "open"
	// chaosHotPath is the file that the overlapping requests change.
	chaosHotPath = "chaos/hot.txt"
	// chaosWriterGap is the shortest pause between two updates of the external
	// writer. The writer draws each pause from chaosWriterGap to four times
	// that, so it moves the branch occasionally and not in a tight loop.
	chaosWriterGap = 100 * time.Millisecond
	// chaosWriterWins is the number of updates that win the compare-and-swap
	// before the external writer stops. A bounded count leaves the lanes free
	// for most of the run: an unbounded writer moves the branch faster than a
	// batch attempt can land, so the lanes give up with "kept moving".
	chaosWriterWins = 3
)

// chaosBranches lists the branches of the scenario. The first two are
// protected: only the workers may land on them.
var chaosBranches = []string{"main", "release", "open"}

// chaosProtected reports the protected branches.
var chaosProtected = map[string]bool{"main": true, "release": true}

// chaosCancel says how a request is cancelled.
type chaosCancel int

const (
	// chaosNoCancel leaves the request alone.
	chaosNoCancel chaosCancel = iota
	// chaosCancelBeforeCall cancels the context before the call is made.
	chaosCancelBeforeCall
	// chaosCancelInFlight gives the call a deadline a few milliseconds away,
	// so it is usually cancelled while the server works on it.
	chaosCancelInFlight
)

// chaosRequest is one planned landing. The runner fills in commit and blob
// after it has created the commit.
type chaosRequest struct {
	agent       string
	branch      string
	path        string
	content     string
	overlapping bool
	resolve     bool
	cancel      chaosCancel

	commit string
	blob   string
}

func (r chaosRequest) protected() bool { return chaosProtected[r.branch] }
func (r chaosRequest) outsider() bool  { return r.agent == chaosOutsider }
func (r chaosRequest) cancelled() bool { return r.cancel != chaosNoCancel }

// chaosResult is the outcome of one request.
type chaosResult struct {
	landed string
	// resolved is the number of paths that the resolver resolved for the land.
	resolved int
	err      error
}

// chaosAgentName returns the name of agent i. The last agent is the outsider.
func chaosAgentName(i int) string {
	if i == chaosAgents-1 {
		return chaosOutsider
	}
	return workerName(i)
}

// chaosGraft returns the configuration of the scenario and the tokens of its
// agents. The workers may land on the protected branches, the outsider may
// not, and every agent may land on the open branch. A resolver is configured,
// so the requests with ResolveConflicts set resolve their overlapping changes.
func chaosGraft(t *testing.T) (config.Graft, map[string]string) {
	t.Helper()

	tokens := map[string]string{}
	agents := make([]config.GraftAgent, 0, chaosAgents)
	for i := 0; i < chaosAgents; i++ {
		name := chaosAgentName(i)
		token := name + "-secret-for-tests"
		tokens[name] = token
		agents = append(agents, config.GraftAgent{Name: name, Token: token})
	}

	graft := config.Graft{
		Standalone: true,
		Agents:     agents,
		Policy: []config.GraftPolicyRule{
			{Actor: "worker-*", Refs: []string{"refs/heads/main", "refs/heads/release"}, Allow: true},
			{Refs: []string{"refs/heads/main", "refs/heads/release"}, Allow: false},
			{Refs: []string{"refs/heads/**"}, Allow: true},
		},
	}
	graft.Land.Batch.Window = "20ms"
	graft.Land.Resolver = config.GraftResolver{
		Enabled: true,
		Command: []string{buildAgentsTool(t, "./cmd/graft-resolve"), "--config", writeChaosResolveConfig(t)},
	}

	return graft, tokens
}

// writeChaosResolveConfig writes the graft-resolve configuration of the
// scenario: the trivial strategy, then the union strategy for the hot file, so
// that the overlapping changes of the scenario are resolved by keeping every
// line.
func writeChaosResolveConfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "resolve.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[[strategy]]
type = "trivial"

[[strategy]]
type = "union"
paths = ["`+chaosHotPath+`"]
`), 0o600))
	return path
}

// planChaos draws the requests of the scenario from rnd. Every agent lands
// once. About one request in five changes the hot file that all overlapping
// requests change, the rest add a file of their own. A branch is drawn for
// each request, and about a quarter of the requests are cancelled.
//
// The first four agents always change the hot file of open and are never
// cancelled. The first two do not resolve, the last two do. Whichever of the
// four lands first creates the hot file, so the others conflict with it: one of
// the first two conflicts, and one of the last two is resolved, whatever the
// order. Every plan exercises both outcomes, whatever the draw.
func planChaos(rnd *rand.Rand) []chaosRequest {
	reqs := make([]chaosRequest, chaosAgents)
	for i := range reqs {
		r := chaosRequest{
			agent:   chaosAgentName(i),
			branch:  chaosBranches[rnd.Intn(len(chaosBranches))],
			resolve: rnd.Intn(2) == 0,
		}
		r.overlapping = rnd.Intn(5) == 0
		if r.overlapping {
			r.path = "chaos/hot.txt"
		} else {
			r.path = fmt.Sprintf("chaos/%02d.txt", i)
		}
		r.content = fmt.Sprintf("%s wrote %d\n", r.agent, i)

		switch rnd.Intn(8) {
		case 0:
			r.cancel = chaosCancelBeforeCall
		case 1:
			r.cancel = chaosCancelInFlight
		}

		reqs[i] = r
	}

	for i := 0; i < 4; i++ {
		reqs[i].branch = chaosExternalBranch
		reqs[i].overlapping = true
		reqs[i].path = chaosHotPath
		reqs[i].resolve = i >= 2
		reqs[i].cancel = chaosNoCancel
	}

	return reqs
}

// runChaosRequest lands req as repo's agent. A cancelled request uses a
// context that is already cancelled, or one with a short deadline.
func runChaosRequest(ctx context.Context, repo *sdk.Repo, req chaosRequest) (*sdk.LandResult, error) {
	var (
		callCtx context.Context
		cancel  context.CancelFunc
	)
	if req.cancel == chaosCancelInFlight {
		callCtx, cancel = context.WithTimeout(ctx, 3*time.Millisecond)
	} else {
		callCtx, cancel = context.WithCancel(ctx)
		if req.cancel == chaosCancelBeforeCall {
			cancel()
		}
	}
	defer cancel()

	return repo.Land(callCtx, sdk.LandOptions{
		Branch:           req.branch,
		Commit:           req.commit,
		User:             sdk.User{Name: clientDisplayName, Email: req.agent + "@example.com"},
		Strategy:         sdk.LandRebase,
		ResolveConflicts: req.resolve,
	})
}

// chaosExternalWriter moves chaosExternalBranch with UserUpdateBranch until it
// has won chaosWriterWins updates or done is closed. The first update comes at
// once, the next ones after a pause drawn from rnd. Each update points the
// branch at a commit with the same tree as the tip, so the files of the lands
// are unaffected. It returns the number of updates that won the
// compare-and-swap. A lost race only ends that attempt. Any other error, a
// denial included, fails the test: the policy admits the writer to every
// branch.
func chaosExternalWriter(t *testing.T, s gitalyStack, done <-chan struct{}, rnd *rand.Rand) int {
	t.Helper()

	client := gitalypb.NewOperationServiceClient(s.dialAgent(t, sharedTestToken))
	repo := &gitalypb.Repository{StorageName: s.storage, RelativePath: s.relPath}
	user := &gitalypb.User{GlId: "user-external", Name: []byte("external writer"), Email: []byte("external@example.com")}

	won := 0
	var pause time.Duration
	for n := 0; won < chaosWriterWins; n++ {
		select {
		case <-done:
			return won
		case <-time.After(pause):
		}
		pause = chaosWriterGap + time.Duration(rnd.Int63n(int64(3*chaosWriterGap)))

		tip := s.git(t, "rev-parse", "refs/heads/"+chaosExternalBranch)
		next := s.git(t, "commit-tree", tip+"^{tree}", "-p", tip, "-m", fmt.Sprintf("external writer %d", n))

		_, err := client.UserUpdateBranch(s.ctx, &gitalypb.UserUpdateBranchRequest{
			Repository: repo,
			BranchName: []byte(chaosExternalBranch),
			User:       user,
			Newrev:     []byte(next),
			Oldrev:     []byte(tip),
		})
		switch sdk.Code(err) {
		case codes.OK:
			won++
		case codes.FailedPrecondition:
			// A land moved the branch first, so the update lost the race.
		default:
			require.NoError(t, err, "the external writer updates %s", chaosExternalBranch)
		}
	}

	return won
}

// chaosLogEntry is a changeset of the log, reduced to what the invariants
// read.
type chaosLogEntry struct {
	Reference string
	Operation string
	Actor     string
	Members   []chaosMember
}

// chaosMember is a member of a changeset.
type chaosMember struct {
	Original string
	Landed   string
	Actor    string
}

// chaosView is the state of the repository that the invariants read.
type chaosView struct {
	// reachable maps a branch to the commits reachable from its tip.
	reachable map[string]map[string]bool
	// merges maps a branch to the number of merge commits between the base
	// and its tip, resolution merges excluded.
	merges map[string]int
	// tree maps a branch to the blob of each path of its tree.
	tree map[string]map[string]string
	// hot maps a branch to the content of the hot file of its tip, when the
	// tip has one.
	hot map[string]string
	// log is the changeset log.
	log []chaosLogEntry
}

// chaosCancellation reports whether err is the error of a cancelled or expired
// call. The server does not stop a member that is already in a batch when its
// submitter gives up, so a cancelled request may still have landed.
func chaosCancellation(err error) bool {
	code := sdk.Code(err)
	return code == codes.Canceled || code == codes.DeadlineExceeded
}

// chaosFailureAllowed reports whether err is an outcome that req may have. A
// kept-moving error is allowed only on the branch the external writer moves,
// since the lane gives up after that branch moved too often. A merge conflict
// needs an overlapping change, a denial needs the outsider on a protected
// branch, and a cancellation needs a cancelled request.
func chaosFailureAllowed(req chaosRequest, err error) bool {
	if strings.Contains(err.Error(), "kept moving") {
		return req.branch == chaosExternalBranch
	}

	switch {
	case sdk.IsMergeConflict(err):
		return req.overlapping
	case sdk.Code(err) == codes.PermissionDenied:
		return req.outsider() && req.protected()
	case chaosCancellation(err):
		return req.cancelled()
	}

	return false
}

// chaosCoverage counts the changeset log entries that record the landing of
// req on ref. A member counts when it names the submitted commit and its actor
// is the agent. A changeset without members counts when it is a land of the
// agent on ref.
func chaosCoverage(log []chaosLogEntry, ref string, req chaosRequest) int {
	count := 0
	for _, entry := range log {
		if len(entry.Members) == 0 {
			if entry.Reference == ref && entry.Operation == "land" && entry.Actor == req.agent {
				count++
			}
			continue
		}

		for _, m := range entry.Members {
			if m.Original == req.commit && m.Actor == req.agent {
				count++
			}
		}
	}

	return count
}

// checkChaosInvariants returns the violated invariants, one message each. The
// invariants are: the rebase lanes stay linear, apart from the resolution
// merges that the resolver writes; a successful landing is reachable from its
// branch tip, its file is in the tree, and it is covered by the changeset log
// exactly once. A disjoint file holds the submitted blob. An overlapping file
// is shared with the other overlapping changes of its branch, and later
// resolutions rewrite it, so it must hold the line of the submitted change
// instead. A failed request has a failure that its kind allows, and is covered
// by the changeset log at most once. Only a cancelled request may be covered
// after it failed: the call returned before the batch that holds it had
// finished. Every other failure is covered by no changeset. The outsider never
// lands on a protected branch: it neither succeeds there nor is covered there,
// cancelled or not. A success resolves paths only if its request asked for
// conflict resolution.
func checkChaosInvariants(reqs []chaosRequest, results []chaosResult, view chaosView) []string {
	var violations []string
	fail := func(format string, args ...any) {
		violations = append(violations, fmt.Sprintf(format, args...))
	}

	for _, branch := range chaosBranches {
		if n := view.merges[branch]; n != 0 {
			fail("branch %s has %d merge commits, the rebase lanes must stay linear", branch, n)
		}
	}

	for i, req := range reqs {
		res := results[i]
		ref := "refs/heads/" + req.branch
		covered := chaosCoverage(view.log, ref, req)

		// The policy denies the outsider every protected branch, whatever the
		// path of the request: batched, batch of one or unbatched. A
		// cancelled call may return before its batch finishes, but the
		// change must never land, so the outsider is never in the log of a
		// protected branch.
		if req.outsider() && req.protected() && covered > 0 {
			fail("%s on protected %s is in the changeset log %d times, the policy must deny it", req.agent, req.branch, covered)
		}

		if res.err != nil {
			if !chaosFailureAllowed(req, res.err) {
				fail("%s on %s failed with %v, which is not an outcome of this request", req.agent, req.branch, res.err)
			}
			switch {
			case covered > 1:
				fail("%s on %s failed but is in the changeset log %d times", req.agent, req.branch, covered)
			case covered == 1 && !chaosCancellation(res.err):
				fail("%s on %s failed with %v but is in the changeset log", req.agent, req.branch, res.err)
			}
			continue
		}

		if req.outsider() && req.protected() {
			fail("%s landed on protected %s, the policy must deny it", req.agent, req.branch)
		}
		if res.resolved > 0 && !req.resolve {
			fail("%s on %s resolved %d paths without asking for conflict resolution", req.agent, req.branch, res.resolved)
		}
		if res.landed == "" {
			fail("%s on %s succeeded without a landed commit", req.agent, req.branch)
			continue
		}
		if !view.reachable[req.branch][res.landed] {
			fail("%s: landed commit %s is not reachable from %s", req.agent, res.landed, req.branch)
		}
		if req.overlapping {
			if !chaosHasLine(view.hot[req.branch], strings.TrimSuffix(req.content, "\n")) {
				fail("%s: the line of the submitted change is missing from %s on %s", req.agent, req.path, req.branch)
			}
		} else if got := view.tree[req.branch][req.path]; got != req.blob {
			fail("%s: %s on %s holds blob %q, want the submitted blob %q", req.agent, req.path, req.branch, got, req.blob)
		}
		if covered != 1 {
			fail("%s on %s is covered %d times by the changeset log, want once", req.agent, req.branch, covered)
		}
	}

	return violations
}

// chaosHasLine reports whether text has a line equal to line.
func chaosHasLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

// chaosViewOf reads the view of the repository from the server.
func chaosViewOf(t *testing.T, s gitalyStack, repo *sdk.Repo, base string) chaosView {
	t.Helper()

	view := chaosView{
		reachable: map[string]map[string]bool{},
		merges:    map[string]int{},
		tree:      map[string]map[string]string{},
		hot:       map[string]string{},
	}
	for _, branch := range chaosBranches {
		ref := "refs/heads/" + branch

		view.reachable[branch] = map[string]bool{}
		for _, commit := range strings.Fields(s.git(t, "rev-list", ref)) {
			view.reachable[branch][commit] = true
		}

		for _, merge := range strings.Fields(s.git(t, "rev-list", "--min-parents=2", base+".."+ref)) {
			if !strings.Contains(s.git(t, "log", "-1", "--format=%B", merge), "Graft-Resolved-By:") {
				view.merges[branch]++
			}
		}

		view.tree[branch] = map[string]string{}
		for _, line := range strings.Split(s.git(t, "ls-tree", "-r", ref), "\n") {
			meta, path, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			if fields := strings.Fields(meta); len(fields) == 3 {
				view.tree[branch][path] = fields[2]
			}
		}
		if _, ok := view.tree[branch][chaosHotPath]; ok {
			view.hot[branch] = s.git(t, "show", ref+":"+chaosHotPath)
		}
	}

	for _, cs := range allChangesets(s.ctx, t, repo) {
		entry := chaosLogEntry{
			Reference: string(cs.GetReference()),
			Operation: cs.GetOperation(),
			Actor:     cs.GetActor(),
		}
		for _, m := range cs.GetMembers() {
			entry.Members = append(entry.Members, chaosMember{
				Original: m.GetOriginalCommitId(),
				Landed:   m.GetCommitId(),
				Actor:    m.GetActor(),
			})
		}
		view.log = append(view.log, entry)
	}

	return view
}

// chaosSeedFromEnv returns the seed of the plan: chaosSeed, unless
// GRAFT_CHAOS_SEED names another one.
func chaosSeedFromEnv(t *testing.T) int64 {
	t.Helper()

	value := os.Getenv(chaosSeedEnv)
	if value == "" {
		return chaosSeed
	}

	seed, err := strconv.ParseInt(value, 10, 64)
	require.NoError(t, err, "%s must be an integer seed", chaosSeedEnv)

	return seed
}

// TestChaosLanding runs the chaos scenario and checks its invariants. It is
// skipped with -short. A failing run logs the seed of its plan.
func TestChaosLanding(t *testing.T) {
	if testing.Short() {
		t.Skip("the chaos scenario runs for about a minute")
	}
	t.Parallel()

	seed := chaosSeedFromEnv(t)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the chaos plan used seed %d, replay the plan with %s=%d", seed, chaosSeedEnv, seed)
		}
	})

	reqs := planChaos(rand.New(rand.NewSource(seed)))
	graft, tokens := chaosGraft(t)
	s := startGitaly(t, graft, sharedTestToken)
	ctx := s.ctx

	base := writeMain(t, s, map[string]string{sharedLogPath: sharedLogHeader})
	for _, branch := range chaosBranches[1:] {
		s.git(t, "update-ref", "refs/heads/"+branch, base)
	}

	repos := make([]*sdk.Repo, len(reqs))
	for i := range reqs {
		repos[i] = s.repoAs(t, tokens[reqs[i].agent])
		var err error
		reqs[i].commit, err = commitFile(ctx, repos[i], reqs[i].agent, "topic/chaos-"+reqs[i].agent, base, reqs[i].path, reqs[i].content)
		require.NoError(t, err)
		reqs[i].blob = s.git(t, "rev-parse", reqs[i].commit+":"+reqs[i].path)
	}

	// The budget bounds the lands. A land that outlives it fails with a
	// deadline error, which the invariants reject.
	landCtx, cancelLands := context.WithTimeout(ctx, chaosBudget)
	defer cancelLands()

	results := make([]chaosResult, len(reqs))
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			res, err := runChaosRequest(landCtx, repos[i], reqs[i])
			results[i] = chaosResult{err: err}
			if res != nil {
				results[i].landed = res.LandedCommitID
				results[i].resolved = len(res.ResolvedPaths)
			}
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// The external writer runs here, in the test goroutine, until it has won its
	// updates. The lands may outlive it, so the results are read after done.
	externalWins := chaosExternalWriter(t, s, done, rand.New(rand.NewSource(seed+1)))
	t.Logf("the external writer won %d updates of %s", externalWins, chaosExternalBranch)
	<-done

	var successes, openSuccesses, conflicts, resolutions int
	for i, res := range results {
		switch {
		case res.err == nil:
			successes++
			resolutions += res.resolved
			if reqs[i].branch == chaosExternalBranch {
				openSuccesses++
			}
		case sdk.IsMergeConflict(res.err):
			conflicts++
		}
		if res.err != nil {
			t.Logf("%s on %s failed with %v", reqs[i].agent, reqs[i].branch, res.err)
		}
	}
	t.Logf("%d lands succeeded, %d on %s, %d conflicted, and %d paths were resolved", successes, openSuccesses, chaosExternalBranch, conflicts, resolutions)
	require.Positive(t, successes, "some lands succeed")
	require.Positive(t, openSuccesses, "some lands succeed on %s, the branch the external writer moves", chaosExternalBranch)
	require.Positive(t, conflicts, "some overlapping lands conflict")
	require.Positive(t, resolutions, "the resolver resolves some overlapping lands")
	require.Positive(t, externalWins, "the external writer wins an update of %s", chaosExternalBranch)

	view := chaosViewOf(t, s, s.repoAs(t, tokens[workerName(0)]), base)
	require.Empty(t, checkChaosInvariants(reqs, results, view), "the chaos invariants hold")
}

// TestChaosPlanIsSeeded checks that the plan of the scenario depends on the
// seed only, and that it has the outsider and every kind of request.
func TestChaosPlanIsSeeded(t *testing.T) {
	t.Parallel()

	first := planChaos(rand.New(rand.NewSource(chaosSeed)))
	require.True(t, reflect.DeepEqual(first, planChaos(rand.New(rand.NewSource(chaosSeed)))), "the same seed gives the same plan")
	require.Len(t, first, chaosAgents)

	var outsiders int
	for _, req := range first {
		if req.outsider() {
			outsiders++
		}
	}
	require.Equal(t, 1, outsiders, "one request comes from the outsider")
}

// chaosFakeScenario returns a valid outcome of three requests and the view of
// the repository it leaves: a disjoint landing on open, a cancelled request
// that failed, and the outsider's denial on main.
func chaosFakeScenario() ([]chaosRequest, []chaosResult, chaosView) {
	reqs := []chaosRequest{
		{agent: "worker-00", branch: "open", path: "chaos/00.txt", commit: "c0", blob: "b0"},
		{agent: "worker-01", branch: "open", path: "chaos/01.txt", commit: "c1", blob: "b1", cancel: chaosCancelBeforeCall},
		{agent: chaosOutsider, branch: "main", path: "chaos/02.txt", commit: "c2", blob: "b2"},
	}
	results := []chaosResult{
		{landed: "l0"},
		{err: status.Error(codes.Canceled, "context canceled")},
		{err: status.Error(codes.PermissionDenied, "denied")},
	}
	view := chaosView{
		reachable: map[string]map[string]bool{
			"main":    {},
			"release": {},
			"open":    {"l0": true, "x": true},
		},
		merges: map[string]int{},
		tree: map[string]map[string]string{
			"main":    {},
			"release": {},
			"open":    {"chaos/00.txt": "b0"},
		},
		log: []chaosLogEntry{
			{
				Reference: "refs/heads/open",
				Operation: "land_batch",
				Actor:     "graft-batch",
				Members:   []chaosMember{{Original: "c0", Landed: "l0", Actor: "worker-00"}},
			},
		},
	}

	return reqs, results, view
}

// TestChaosInvariantCheckerDetectsViolations checks the invariant checker on
// a valid outcome and on one mutation of it per invariant, with fakes.
func TestChaosInvariantCheckerDetectsViolations(t *testing.T) {
	t.Parallel()

	reqs, results, view := chaosFakeScenario()
	require.Empty(t, checkChaosInvariants(reqs, results, view), "the valid outcome holds")

	// A cancelled request may have landed although its call failed.
	view.log = append(view.log, chaosLogEntry{
		Reference: "refs/heads/open",
		Operation: "land",
		Actor:     "worker-01",
	})
	require.Empty(t, checkChaosInvariants(reqs, results, view), "a cancelled request that landed is allowed")
	view.log = view.log[:len(view.log)-1]

	// A request that asked for resolution may report resolved paths.
	reqs[0].resolve = true
	results[0].resolved = 1
	require.Empty(t, checkChaosInvariants(reqs, results, view), "a resolution that was asked for is allowed")
	reqs[0].resolve = false
	results[0].resolved = 0

	for _, tc := range []struct {
		desc   string
		mutate func(reqs []chaosRequest, results []chaosResult, view *chaosView)
	}{
		{
			desc: "a landed commit is not reachable",
			mutate: func(_ []chaosRequest, _ []chaosResult, view *chaosView) {
				delete(view.reachable["open"], "l0")
			},
		},
		{
			desc: "a landed file is missing from the tree",
			mutate: func(_ []chaosRequest, _ []chaosResult, view *chaosView) {
				delete(view.tree["open"], "chaos/00.txt")
			},
		},
		{
			desc: "a success is not in the changeset log",
			mutate: func(_ []chaosRequest, _ []chaosResult, view *chaosView) {
				view.log = nil
			},
		},
		{
			desc: "a success is in the changeset log twice",
			mutate: func(_ []chaosRequest, _ []chaosResult, view *chaosView) {
				view.log = append(view.log, view.log[0])
			},
		},
		{
			desc: "a denied request is in the changeset log",
			mutate: func(_ []chaosRequest, _ []chaosResult, view *chaosView) {
				view.log = append(view.log, chaosLogEntry{
					Reference: "refs/heads/main",
					Operation: "land",
					Actor:     chaosOutsider,
				})
			},
		},
		{
			desc: "a failed request is in the changeset log twice",
			mutate: func(_ []chaosRequest, _ []chaosResult, view *chaosView) {
				entry := chaosLogEntry{
					Reference: "refs/heads/open",
					Operation: "land",
					Actor:     "worker-01",
				}
				view.log = append(view.log, entry, entry)
			},
		},
		{
			desc: "the rebase lane has a merge commit",
			mutate: func(_ []chaosRequest, _ []chaosResult, view *chaosView) {
				view.merges["open"] = 1
			},
		},
		{
			desc: "a failure has a code its request cannot produce",
			mutate: func(_ []chaosRequest, results []chaosResult, _ *chaosView) {
				results[2] = chaosResult{err: status.Error(codes.Unavailable, "unavailable")}
			},
		},
		{
			desc: "an uncancelled request is denied on an open branch",
			mutate: func(reqs []chaosRequest, results []chaosResult, _ *chaosView) {
				reqs[0].branch = "open"
				results[0] = chaosResult{err: status.Error(codes.PermissionDenied, "denied")}
			},
		},
		{
			desc: "the outsider lands on a protected branch in a batch",
			mutate: func(_ []chaosRequest, results []chaosResult, view *chaosView) {
				results[2] = chaosResult{landed: "l2"}
				view.reachable["main"]["l2"] = true
				view.tree["main"]["chaos/02.txt"] = "b2"
				view.log = append(view.log, chaosLogEntry{
					Reference: "refs/heads/main",
					Operation: "land_batch",
					Actor:     "graft-batch",
					Members:   []chaosMember{{Original: "c2", Landed: "l2", Actor: chaosOutsider}},
				})
			},
		},
		{
			desc: "the outsider lands on a protected branch as a batch of one",
			mutate: func(_ []chaosRequest, results []chaosResult, view *chaosView) {
				results[2] = chaosResult{landed: "l2"}
				view.reachable["main"]["l2"] = true
				view.tree["main"]["chaos/02.txt"] = "b2"
				view.log = append(view.log, chaosLogEntry{
					Reference: "refs/heads/main",
					Operation: "land",
					Actor:     chaosOutsider,
					Members:   []chaosMember{{Original: "c2", Landed: "l2", Actor: chaosOutsider}},
				})
			},
		},
		{
			desc: "a cancelled outsider lands on a protected branch",
			mutate: func(reqs []chaosRequest, results []chaosResult, view *chaosView) {
				reqs[2].cancel = chaosCancelInFlight
				results[2] = chaosResult{err: status.Error(codes.DeadlineExceeded, "context deadline exceeded")}
				view.log = append(view.log, chaosLogEntry{
					Reference: "refs/heads/main",
					Operation: "land",
					Actor:     chaosOutsider,
					Members:   []chaosMember{{Original: "c2", Landed: "l2", Actor: chaosOutsider}},
				})
			},
		},
		{
			desc: "a success resolves paths without asking for resolution",
			mutate: func(reqs []chaosRequest, results []chaosResult, _ *chaosView) {
				reqs[0].resolve = false
				results[0].resolved = 2
			},
		},
		{
			desc: "a protected branch keeps moving",
			mutate: func(_ []chaosRequest, results []chaosResult, _ *chaosView) {
				results[2] = chaosResult{err: status.Error(codes.FailedPrecondition, `branch "main" kept moving, giving up after 11 attempts`)}
			},
		},
		{
			desc: "an overlapping landing is missing its line from the hot file",
			mutate: func(reqs []chaosRequest, _ []chaosResult, view *chaosView) {
				reqs[0].overlapping = true
				reqs[0].content = "worker-00 wrote 0\n"
				view.hot = map[string]string{"open": "worker-07 wrote 7\n"}
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			reqs, results, view := chaosFakeScenario()
			tc.mutate(reqs, results, &view)
			require.NotEmpty(t, checkChaosInvariants(reqs, results, view), "the mutation is detected")
		})
	}
}
