package rebase

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
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

			plan, err := planLanding(tip, commit, tc.rng)
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
	plan, err := planLanding("tip", commit, []ancestry.RangeCommit{
		{ID: commit, Parents: []git.ObjectID{"a"}},
		{ID: "a", Parents: []git.ObjectID{commit}},
	})
	require.NoError(t, err)

	_, err = plan.Replay()
	require.ErrorContains(t, err, "cycle")

	plan, err = planLanding("tip", commit, []ancestry.RangeCommit{
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

// increasingDates returns committer dates that grow with the index of the commit, so that every
// commit is younger than its parents.
func increasingDates(n int) []int64 {
	dates := make([]int64, n)
	for i := range dates {
		dates[i] = 1600000000 + int64(i)
	}
	return dates
}

// skewedDates returns increasing dates in which random runs of commits are dated a year before
// the first commit, as commits written by a client whose clock is behind or whose dates were set
// explicitly. git rev-list a..b then lists commits that a reaches.
func skewedDates(rnd *rand.Rand, n int) []int64 {
	dates := increasingDates(n)
	for run := 0; run < n/16; run++ {
		start := rnd.Intn(n)
		for k := 0; k < 6+rnd.Intn(12) && start+k < n; k++ {
			dates[start+k] = 1568000000 + int64(k)
		}
	}
	return dates
}

// writeForest creates the commits with git fast-import. Commit i has the parents parents[i] and the
// committer date dates[i], and is reachable as refs/heads/c<i>.
func writeForest(t *testing.T, cfg config.Cfg, repoPath string, parents [][]int, dates []int64) []git.ObjectID {
	t.Helper()

	var stream bytes.Buffer
	for i, list := range parents {
		name := fmt.Sprintf("c%d", i)
		fmt.Fprintf(&stream, "commit refs/heads/%s\n", name)
		fmt.Fprintf(&stream, "mark :%d\n", i+1)
		fmt.Fprintf(&stream, "committer Tester <tester@example.com> %d +0000\n", dates[i])
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

// exactDecision is how a commit lands onto a tip, computed from the history itself: the ancestors of
// both commits, from the parents that the history was written with.
type exactDecision struct {
	kind      LandKind
	replayErr error
	todo      []git.ObjectID
}

// ancestorsOf returns the indexes of the commits reachable from commit x, x included.
func ancestorsOf(parents [][]int, x int) map[int]bool {
	seen := map[int]bool{x: true}
	stack := []int{x}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range parents[c] {
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return seen
}

func decideExactly(parents [][]int, ids []git.ObjectID, tip, commit int) exactDecision {
	tipAncestors, commitAncestors := ancestorsOf(parents, tip), ancestorsOf(parents, commit)
	switch {
	case tipAncestors[commit]:
		return exactDecision{kind: LandAlreadyLanded}
	case commitAncestors[tip]:
		return exactDecision{kind: LandFastForward}
	}

	inRange := map[int]bool{}
	shared := false
	for x := range commitAncestors {
		if tipAncestors[x] {
			shared = true
		} else {
			inRange[x] = true
		}
	}
	for x := range inRange {
		if len(parents[x]) > 1 {
			return exactDecision{kind: LandDiverged, replayErr: ErrMergesInRange}
		}
	}
	if !shared {
		return exactDecision{kind: LandDiverged, replayErr: ErrUnrelated}
	}

	// Without merge commits the range is the chain from commit down to the history of tip.
	var todo []git.ObjectID
	for x := commit; inRange[x]; x = parents[x][0] {
		todo = append([]git.ObjectID{ids[x]}, todo...)
	}
	return exactDecision{kind: LandDiverged, todo: todo}
}

// TestPlanMatchesTheExactHistory compares, on seeded random forests, the plan with the decision
// computed from the history itself, for the git and the graph checker, with committer dates that
// grow along the history and with skewed ones. The kinds are also checked against git merge-base
// --is-ancestor, so that the history the test computes from is the one git sees.
func TestPlanMatchesTheExactHistory(t *testing.T) {
	t.Parallel()

	for _, skewed := range []bool{false, true} {
		for _, seed := range []int64{1, 2, 3} {
			t.Run(fmt.Sprintf("skewed %v seed %d", skewed, seed), func(t *testing.T) {
				t.Parallel()

				f := newFixture(t)
				rnd := rand.New(rand.NewSource(seed))
				parents := forestHistory(rnd, 160)
				dates := increasingDates(len(parents))
				if skewed {
					dates = skewedDates(rnd, len(parents))
				}
				ids := writeForest(t, f.cfg, f.repoPath, parents, dates)
				index := make(map[git.ObjectID]int, len(ids))
				for i, id := range ids {
					index[id] = i
				}

				catfileCache := catfile.NewCache(f.cfg)
				t.Cleanup(catfileCache.Stop)
				reader, cancel, err := catfileCache.ObjectReader(f.ctx, f.repo)
				require.NoError(t, err)
				t.Cleanup(cancel)

				graphChecker, err := ancestry.Graph(f.ctx, f.repo, reader, derived.NewStore(f.repoPath))
				require.NoError(t, err)

				checkers := map[string]ancestry.Checker{"git": ancestry.Git(f.repo), "graph": graphChecker}

				isAncestor := func(a, b git.ObjectID) bool {
					cmd := gittest.NewCommand(t, f.cfg, "-C", f.repoPath, "merge-base", "--is-ancestor", a.String(), b.String())
					if err := cmd.Run(); err != nil {
						require.Equal(t, 1, cmd.ProcessState.ExitCode())
						return false
					}
					return true
				}

				type pair struct{ tip, commit int }
				pairs := []pair{{3, 4}, {4, 3}, {3, 3}, {0, len(ids) - 1}}
				for range 200 {
					pairs = append(pairs, pair{rnd.Intn(len(ids)), rnd.Intn(len(ids))})
				}

				seen := map[string]int{}
				overListed := 0
				for _, p := range pairs {
					tip, commit := ids[p.tip], ids[p.commit]
					want := decideExactly(parents, ids, p.tip, p.commit)
					require.Equal(t, want.kind == LandAlreadyLanded, isAncestor(commit, tip))
					require.Equal(t, want.kind == LandFastForward, !isAncestor(commit, tip) && isAncestor(tip, commit))

					listed, err := checkers["git"].List(f.ctx, commit, []git.ObjectID{tip})
					require.NoError(t, err)
					tipAncestors := ancestorsOf(parents, p.tip)
					for _, c := range listed {
						if tipAncestors[index[c.ID]] {
							overListed++
							break
						}
					}

					for name, checker := range checkers {
						plan, err := Plan(f.ctx, checker, tip, commit)
						require.NoError(t, err)
						require.Equal(t, want.kind, plan.Kind(), "%s: kind of c%d onto c%d", name, p.commit, p.tip)

						if want.kind != LandDiverged {
							continue
						}

						todo, err := plan.Replay()
						if want.replayErr != nil {
							require.ErrorIs(t, err, want.replayErr, "%s: replay of c%d onto c%d", name, p.commit, p.tip)
							continue
						}
						require.NoError(t, err)
						require.Equal(t, want.todo, todo, "%s: todo of c%d onto c%d", name, p.commit, p.tip)
					}

					label := fmt.Sprint(want.kind)
					if want.replayErr != nil {
						label += want.replayErr.Error()
					}
					seen[label]++
				}

				// The forests must exercise every decision, and the skewed ones listings that git
				// rev-list gets wrong.
				require.Len(t, seen, 5, "decisions seen: %v", seen)
				if skewed {
					require.NotZero(t, overListed, "no listing of git rev-list held a commit outside of its range")
				} else {
					require.Zero(t, overListed)
				}
			})
		}
	}
}

// overListingChecker answers from a history held in memory. Its List lists every commit that head
// reaches without going through an excluded commit, the commits that the excluded commits reach
// included: the most that git rev-list can list under committer-date skew, since git marks the
// excluded commits as excluded from the start and its walk from head stops at them.
type overListingChecker struct {
	parents [][]int
	ids     []git.ObjectID
	index   map[git.ObjectID]int
}

func (c overListingChecker) List(_ context.Context, head git.ObjectID, exclude []git.ObjectID) ([]ancestry.RangeCommit, error) {
	excluded := map[int]bool{}
	for _, id := range exclude {
		excluded[c.index[id]] = true
	}
	if excluded[c.index[head]] {
		return nil, nil
	}

	seen := map[int]bool{c.index[head]: true}
	stack := []int{c.index[head]}
	var commits []ancestry.RangeCommit
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		var parents []git.ObjectID
		for _, p := range c.parents[x] {
			parents = append(parents, c.ids[p])
			if !seen[p] && !excluded[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
		commits = append(commits, ancestry.RangeCommit{ID: c.ids[x], Parents: parents})
	}
	return commits, nil
}

func (c overListingChecker) ReachedBottoms(ctx context.Context, base git.ObjectID, bottoms []git.ObjectID) (map[git.ObjectID]bool, error) {
	return c.Reachable(ctx, base, bottoms)
}

func (c overListingChecker) Reachable(_ context.Context, base git.ObjectID, commits []git.ObjectID) (map[git.ObjectID]bool, error) {
	ancestors := ancestorsOf(c.parents, c.index[base])
	reached := map[git.ObjectID]bool{}
	for _, id := range commits {
		if ancestors[c.index[id]] {
			reached[id] = true
		}
	}
	return reached, nil
}

func (c overListingChecker) MergeBases(_ context.Context, a, b git.ObjectID) ([]git.ObjectID, error) {
	aAncestors, bAncestors := ancestorsOf(c.parents, c.index[a]), ancestorsOf(c.parents, c.index[b])
	var bases []git.ObjectID
	for x := range aAncestors {
		if !bAncestors[x] {
			continue
		}
		best := true
		for y := range aAncestors {
			if y != x && bAncestors[y] && ancestorsOf(c.parents, y)[x] {
				best = false
				break
			}
		}
		if best {
			bases = append(bases, c.ids[x])
		}
	}
	return bases, nil
}

// TestPlanWithAListingOfTheWholeHistory plans every pair of random forests with a checker whose
// listing holds the whole history of the commit, the shared part included: the plan must still
// match the exact decision.
func TestPlanWithAListingOfTheWholeHistory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	seen := map[string]int{}
	for _, seed := range []int64{1, 2, 3} {
		rnd := rand.New(rand.NewSource(seed))
		parents := forestHistory(rnd, 80)
		ids := make([]git.ObjectID, len(parents))
		index := make(map[git.ObjectID]int, len(parents))
		for i := range ids {
			ids[i] = git.ObjectID(fmt.Sprintf("c%02d", i))
			index[ids[i]] = i
		}
		checker := overListingChecker{parents: parents, ids: ids, index: index}

		for tip := range ids {
			for commit := range ids {
				want := decideExactly(parents, ids, tip, commit)

				plan, err := Plan(ctx, checker, ids[tip], ids[commit])
				require.NoError(t, err)
				require.Equal(t, want.kind, plan.Kind(), "seed %d: kind of c%d onto c%d", seed, commit, tip)

				label := fmt.Sprint(want.kind)
				if want.kind == LandDiverged {
					todo, err := plan.Replay()
					if want.replayErr != nil {
						require.ErrorIs(t, err, want.replayErr, "seed %d: replay of c%d onto c%d", seed, commit, tip)
						label += want.replayErr.Error()
					} else {
						require.NoError(t, err)
						require.Equal(t, want.todo, todo, "seed %d: todo of c%d onto c%d", seed, commit, tip)
					}
				}
				seen[label]++
			}
		}
	}
	require.Len(t, seen, 5, "decisions seen: %v", seen)
}
