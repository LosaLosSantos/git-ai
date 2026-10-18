package rebase

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/ancestry"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/stretchr/testify/require"
)

func TestPlanLanding(t *testing.T) {
	t.Parallel()

	// Synthetic IDs: the plan only compares them.
	id := func(name string) git.ObjectID { return git.ObjectID(name) }
	tip, commit := id("tip"), id("commit")
	rc := func(name string, parents ...string) ancestry.RangeCommit {
		ps := make([]git.ObjectID, 0, len(parents))
		for _, p := range parents {
			ps = append(ps, id(p))
		}
		return ancestry.RangeCommit{ID: id(name), Parents: ps}
	}

	for _, tc := range []struct {
		desc       string
		rng        []ancestry.RangeCommit
		wantKind   LandKind
		wantTodo   []git.ObjectID
		wantReplay error
		wantErr    string
	}{
		{
			desc:     "empty range is already landed",
			wantKind: LandAlreadyLanded,
		},
		{
			desc:     "tip as parent is a fast-forward",
			rng:      []ancestry.RangeCommit{rc("commit", "c1"), rc("c1", "tip")},
			wantKind: LandFastForward,
		},
		{
			desc:     "fast-forward with a merge commit in the range",
			rng:      []ancestry.RangeCommit{rc("commit", "m"), rc("m", "a", "b"), rc("a", "tip"), rc("b", "base")},
			wantKind: LandFastForward,
		},
		{
			desc:     "diverged chain replays oldest first",
			rng:      []ancestry.RangeCommit{rc("commit", "c2"), rc("c1", "base"), rc("c2", "c1")},
			wantKind: LandDiverged,
			wantTodo: []git.ObjectID{id("c1"), id("c2"), commit},
		},
		{
			desc:       "merge commit in a diverged range",
			rng:        []ancestry.RangeCommit{rc("commit", "a", "b"), rc("a", "base"), rc("b", "base")},
			wantKind:   LandDiverged,
			wantReplay: ErrMergesInRange,
		},
		{
			desc:       "merges are reported before unrelated history",
			rng:        []ancestry.RangeCommit{rc("commit", "a", "b"), rc("a"), rc("b")},
			wantKind:   LandDiverged,
			wantReplay: ErrMergesInRange,
		},
		{
			desc:       "chain down to a root is unrelated",
			rng:        []ancestry.RangeCommit{rc("commit", "c1"), rc("c1")},
			wantKind:   LandDiverged,
			wantReplay: ErrUnrelated,
		},
		{
			desc:       "single root commit is unrelated",
			rng:        []ancestry.RangeCommit{rc("commit")},
			wantKind:   LandDiverged,
			wantReplay: ErrUnrelated,
		},
		{
			desc:    "range without the commit",
			rng:     []ancestry.RangeCommit{rc("other", "base")},
			wantErr: "does not contain the commit",
		},
		{
			desc:    "commit listed twice",
			rng:     []ancestry.RangeCommit{rc("commit", "base"), rc("commit", "base")},
			wantErr: "twice",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			plan, err := PlanLanding(tip, commit, tc.rng)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantKind, plan.Kind())

			if tc.wantKind != LandDiverged {
				_, err := plan.Replay()
				require.Error(t, err, "only a diverged plan replays")
				return
			}

			todo, err := plan.Replay()
			if tc.wantReplay != nil {
				require.ErrorIs(t, err, tc.wantReplay)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantTodo, todo)
		})
	}
}

func TestPlanLandingRejectsRangesThatAreNotHistories(t *testing.T) {
	t.Parallel()

	commit := git.ObjectID("commit")
	plan, err := PlanLanding("tip", commit, []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"a"}},
		{ID: "a", Parents: []git.ObjectID{commit}},
	})
	require.NoError(t, err)

	_, err = plan.Replay()
	require.ErrorContains(t, err, "cycle")

	plan, err = PlanLanding("tip", commit, []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"base"}},
		{ID: "stray", Parents: []git.ObjectID{"base"}},
	})
	require.NoError(t, err)

	_, err = plan.Replay()
	require.ErrorContains(t, err, "not a chain")
}

// forestHistory returns the parents of n commits as indexes lower than the commit's own: a forest
// with several roots, long linear stretches, merges of earlier commits and a criss-cross.
func forestHistory(rnd *rand.Rand, n int) [][]int {
	parents := make([][]int, n)
	parents[1] = []int{0}
	parents[2] = []int{0}
	parents[3] = []int{1, 2}
	parents[4] = []int{2, 1}

	for i := 5; i < n; i++ {
		switch r := rnd.Intn(20); {
		case r == 0:
			// A new root: its descendants share no history with the other trees.
		case r < 6:
			first := rnd.Intn(i)
			second := rnd.Intn(i)
			if second == first {
				parents[i] = []int{first}
			} else {
				parents[i] = []int{first, second}
			}
		default:
			if rnd.Intn(3) == 0 {
				parents[i] = []int{rnd.Intn(i)}
			} else {
				parents[i] = []int{i - 1}
			}
		}
	}

	return parents
}

// writeForest creates the commits with git fast-import. Commit i has the parents parents[i] and is
// reachable as refs/heads/c<i>.
func writeForest(t *testing.T, cfg config.Cfg, repoPath string, parents [][]int) []git.ObjectID {
	t.Helper()

	var stream bytes.Buffer
	for i, list := range parents {
		name := fmt.Sprintf("c%d", i)
		fmt.Fprintf(&stream, "commit refs/heads/%s\n", name)
		fmt.Fprintf(&stream, "mark :%d\n", i+1)
		fmt.Fprintf(&stream, "committer Tester <tester@example.com> %d +0000\n", 1600000000+i)
		fmt.Fprintf(&stream, "data %d\n%s\n", len(name), name)
		for j, parent := range list {
			if j == 0 {
				fmt.Fprintf(&stream, "from :%d\n", parent+1)
			} else {
				fmt.Fprintf(&stream, "merge :%d\n", parent+1)
			}
		}
		stream.WriteString("\n")
	}

	gittest.ExecOpts(t, cfg, gittest.ExecConfig{Stdin: &stream}, "-C", repoPath, "fast-import", "--quiet")

	ids := make([]git.ObjectID, 0, len(parents))
	for i := range parents {
		ids = append(ids, gittest.ResolveRevision(t, cfg, repoPath, fmt.Sprintf("refs/heads/c%d", i)))
	}
	return ids
}

// phase4aDecision is how landing decided before the plan, one git command per question, in the
// order of the batch stepper and the engine: already landed, fast-forward, merges in the range,
// common ancestor, and the todo list of the rebase.
type phase4aDecision struct {
	kind      LandKind
	replayErr error
	todo      []git.ObjectID
}

func decideAsPhase4a(t *testing.T, cfg config.Cfg, repoPath string, tip, commit git.ObjectID) phase4aDecision {
	t.Helper()

	exitCode := func(args ...string) int {
		cmd := gittest.NewCommand(t, cfg, append([]string{"-C", repoPath}, args...)...)
		if err := cmd.Run(); err != nil {
			return cmd.ProcessState.ExitCode()
		}
		return 0
	}

	if exitCode("merge-base", "--is-ancestor", commit.String(), tip.String()) == 0 {
		return phase4aDecision{kind: LandAlreadyLanded}
	}
	if exitCode("merge-base", "--is-ancestor", tip.String(), commit.String()) == 0 {
		return phase4aDecision{kind: LandFastForward}
	}

	if out := strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list", "--merges", tip.String()+".."+commit.String()))); out != "" {
		return phase4aDecision{kind: LandDiverged, replayErr: ErrMergesInRange}
	}

	switch code := exitCode("merge-base", "--all", tip.String(), commit.String()); code {
	case 0:
	case 1:
		return phase4aDecision{kind: LandDiverged, replayErr: ErrUnrelated}
	default:
		t.Fatalf("merge-base --all exited with %d", code)
	}

	var todo []git.ObjectID
	for _, line := range strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath,
		"rev-list", "--right-only", "--no-merges", "--topo-order", "--reverse", tip.String()+"..."+commit.String()))) {
		todo = append(todo, git.ObjectID(line))
	}

	return phase4aDecision{kind: LandDiverged, todo: todo}
}

// TestPlanLandingMatchesPhase4aDecisions compares, on seeded random forests, the plan made from one
// range listing with the decisions landing made before it with one git command per question, for
// the git and the graph checker.
func TestPlanLandingMatchesPhase4aDecisions(t *testing.T) {
	t.Parallel()

	for _, seed := range []int64{1, 2, 3} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			rnd := rand.New(rand.NewSource(seed))
			ids := writeForest(t, f.cfg, f.repoPath, forestHistory(rnd, 160))

			catfileCache := catfile.NewCache(f.cfg)
			t.Cleanup(catfileCache.Stop)
			reader, cancel, err := catfileCache.ObjectReader(f.ctx, f.repo)
			require.NoError(t, err)
			t.Cleanup(cancel)

			graphChecker, err := ancestry.Graph(f.ctx, f.repo, reader, derived.NewStore(f.repoPath))
			require.NoError(t, err)

			checkers := map[string]ancestry.Checker{"git": ancestry.Git(f.repo), "graph": graphChecker}

			type pair struct{ tip, commit git.ObjectID }
			pairs := []pair{{ids[3], ids[4]}, {ids[4], ids[3]}, {ids[3], ids[3]}, {ids[0], ids[len(ids)-1]}}
			for range 200 {
				pairs = append(pairs, pair{ids[rnd.Intn(len(ids))], ids[rnd.Intn(len(ids))]})
			}

			seen := map[string]int{}
			for _, p := range pairs {
				want := decideAsPhase4a(t, f.cfg, f.repoPath, p.tip, p.commit)

				for name, checker := range checkers {
					rng, err := checker.Range(f.ctx, p.tip, p.commit)
					require.NoError(t, err)

					plan, err := PlanLanding(p.tip, p.commit, rng)
					require.NoError(t, err)
					require.Equal(t, want.kind, plan.Kind(), "%s: kind of %s onto %s", name, p.commit, p.tip)

					if want.kind != LandDiverged {
						continue
					}

					todo, err := plan.Replay()
					if want.replayErr != nil {
						require.ErrorIs(t, err, want.replayErr, "%s: replay of %s onto %s", name, p.commit, p.tip)
						continue
					}
					require.NoError(t, err)
					require.Equal(t, want.todo, todo, "%s: todo of %s onto %s", name, p.commit, p.tip)
				}

				label := fmt.Sprint(want.kind)
				if want.replayErr != nil {
					label += want.replayErr.Error()
				}
				seen[label]++
			}

			// The forests must exercise every decision.
			require.Len(t, seen, 5, "decisions seen: %v", seen)
		})
	}
}
