package revrange

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/stretchr/testify/require"
)

// modelWalker answers from a history held in memory. Its List adds to the exact range a random part
// of the commits that the excluded commits reach, as git rev-list may under committer-date skew.
type modelWalker struct {
	parents map[git.ObjectID][]git.ObjectID
	rnd     *rand.Rand
	// extra is the probability that List adds a commit that an excluded commit reaches.
	extra float64

	lists, reachedBottoms, reachables, mergeBases int
	// lastExtra is the number of extra commits of the last List.
	lastExtra int
}

func (m *modelWalker) ancestors(ids ...git.ObjectID) map[git.ObjectID]bool {
	seen := map[git.ObjectID]bool{}
	stack := append([]git.ObjectID(nil), ids...)
	for _, id := range ids {
		seen[id] = true
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range m.parents[id] {
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return seen
}

func (m *modelWalker) List(_ context.Context, head git.ObjectID, exclude []git.ObjectID) ([]Commit, error) {
	m.lists++
	excluded := m.ancestors(exclude...)
	m.lastExtra = 0

	var commits []Commit
	for _, id := range sortedKeys(m.ancestors(head)) {
		if excluded[id] {
			if m.rnd.Float64() >= m.extra {
				continue
			}
			m.lastExtra++
		}
		commits = append(commits, Commit{ID: id, Parents: m.parents[id]})
	}
	m.rnd.Shuffle(len(commits), func(i, j int) { commits[i], commits[j] = commits[j], commits[i] })
	return commits, nil
}

func (m *modelWalker) ReachedBottoms(_ context.Context, base git.ObjectID, bottoms []git.ObjectID) (map[git.ObjectID]bool, error) {
	m.reachedBottoms++
	return m.reachable(base, bottoms), nil
}

func (m *modelWalker) Reachable(_ context.Context, base git.ObjectID, commits []git.ObjectID) (map[git.ObjectID]bool, error) {
	m.reachables++
	return m.reachable(base, commits), nil
}

func (m *modelWalker) reachable(base git.ObjectID, commits []git.ObjectID) map[git.ObjectID]bool {
	ancestors := m.ancestors(base)
	reached := map[git.ObjectID]bool{}
	for _, id := range commits {
		if ancestors[id] {
			reached[id] = true
		}
	}
	return reached
}

func (m *modelWalker) MergeBases(_ context.Context, a, b git.ObjectID) ([]git.ObjectID, error) {
	m.mergeBases++
	aAncestors, bAncestors := m.ancestors(a), m.ancestors(b)
	var common []git.ObjectID
	for id := range aAncestors {
		if bAncestors[id] {
			common = append(common, id)
		}
	}

	var best []git.ObjectID
	for _, id := range common {
		redundant := false
		for _, other := range common {
			if other != id && m.ancestors(other)[id] {
				redundant = true
				break
			}
		}
		if !redundant {
			best = append(best, id)
		}
	}
	sort.Slice(best, func(i, j int) bool { return best[i] < best[j] })
	return best, nil
}

func sortedKeys(set map[git.ObjectID]bool) []git.ObjectID {
	keys := make([]git.ObjectID, 0, len(set))
	for id := range set {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// randomHistory returns n commits named c0..c<n-1> whose parents have lower numbers: chains, merges
// and a few further roots.
func randomHistory(rnd *rand.Rand, n int) ([]git.ObjectID, map[git.ObjectID][]git.ObjectID) {
	ids := make([]git.ObjectID, n)
	parents := map[git.ObjectID][]git.ObjectID{}
	for i := range ids {
		ids[i] = git.ObjectID(fmt.Sprintf("c%03d", i))
		if i == 0 || rnd.Intn(30) == 0 {
			continue
		}
		first := i - 1
		if rnd.Intn(3) == 0 {
			first = rnd.Intn(i)
		}
		parents[ids[i]] = []git.ObjectID{ids[first]}
		if rnd.Intn(5) == 0 {
			if second := rnd.Intn(i); second != first {
				parents[ids[i]] = append(parents[ids[i]], ids[second])
			}
		}
	}
	return ids, parents
}

func rangeOf(commits []Commit) map[git.ObjectID][]git.ObjectID {
	set := make(map[git.ObjectID][]git.ObjectID, len(commits))
	for _, c := range commits {
		set[c.ID] = c.Parents
	}
	return set
}

// TestExactOnRandomHistories checks Exact against the exact range on random histories, with
// listings that hold no extra commit, some, and every commit that the base reaches. It also checks
// the costs that the package documents, which do not depend on the size of the history: without
// extra commits one listing and one question for all its bottoms; with them, one question more.
func TestExactOnRandomHistories(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for _, extra := range []float64{0, 0.3, 1} {
		for seed := int64(1); seed <= 3; seed++ {
			t.Run(fmt.Sprintf("extra %v seed %d", extra, seed), func(t *testing.T) {
				t.Parallel()

				rnd := rand.New(rand.NewSource(seed))
				ids, parents := randomHistory(rnd, 150)

				skewed := 0
				for k := 0; k < 300; k++ {
					base, head := ids[rnd.Intn(len(ids))], ids[rnd.Intn(len(ids))]
					m := &modelWalker{parents: parents, rnd: rnd, extra: extra}

					baseAncestors := m.ancestors(base)
					want := map[git.ObjectID][]git.ObjectID{}
					for id := range m.ancestors(head) {
						if !baseAncestors[id] {
							want[id] = parents[id]
						}
					}

					got, err := Exact(ctx, m, base, head)
					require.NoError(t, err)
					require.Equal(t, want, rangeOf(got), "range %s..%s", base, head)

					require.Equal(t, 1, m.lists)
					require.Zero(t, m.mergeBases)
					if len(want) == 0 && m.lastExtra == 0 {
						require.Zero(t, m.reachedBottoms+m.reachables, "an empty listing needs no question")
						continue
					}
					require.Equal(t, 1, m.reachedBottoms)
					if m.lastExtra == 0 {
						require.Zero(t, m.reachables, "an exact listing needs no walk")
						continue
					}

					skewed++
					require.Equal(t, 1, m.reachables)
				}

				if extra == 0 {
					require.Zero(t, skewed)
				} else {
					require.Positive(t, skewed)
				}
			})
		}
	}
}

// TestRefineOfAnEmptyListing needs no question.
func TestRefineOfAnEmptyListing(t *testing.T) {
	t.Parallel()

	m := &modelWalker{rnd: rand.New(rand.NewSource(1))}
	got, err := Refine(context.Background(), m, "a", nil)
	require.NoError(t, err)
	require.Empty(t, got)
	require.Zero(t, m.reachedBottoms+m.reachables+m.lists+m.mergeBases)
}

// TestRefineRejectsListingsThatAreNotHistories reports a listing that lists a commit twice.
func TestRefineRejectsListingsThatAreNotHistories(t *testing.T) {
	t.Parallel()

	m := &modelWalker{rnd: rand.New(rand.NewSource(1))}
	_, err := Refine(context.Background(), m, "a", []Commit{{ID: "b"}, {ID: "b"}})
	require.ErrorContains(t, err, "listed twice")
}

func TestBoundary(t *testing.T) {
	t.Parallel()

	rng := []Commit{
		{ID: "d", Parents: []git.ObjectID{"c", "x"}},
		{ID: "c", Parents: []git.ObjectID{"b"}},
		{ID: "b", Parents: []git.ObjectID{"y"}},
		{ID: "e", Parents: []git.ObjectID{"x"}},
	}
	require.Equal(t, []git.ObjectID{"x", "y"}, Boundary(rng))
	require.Empty(t, Boundary(nil))
}
