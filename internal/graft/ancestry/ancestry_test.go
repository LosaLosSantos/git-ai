package ancestry

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/revrange"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// crissCrossHistory returns the parents of n commits as indexes, lower than the index of the
// commit. Commit 0 is the root. Commits 1 and 2 are children of the root, and commits 3 and 4 merge
// them in both orders, which is a criss-cross: the merge bases of 3 and 4 are 1 and 2. The other
// commits merge random earlier commits.
func crissCrossHistory(rnd *rand.Rand, n int) [][]int {
	parents := make([][]int, n)
	parents[1] = []int{0}
	parents[2] = []int{0}
	parents[3] = []int{1, 2}
	parents[4] = []int{2, 1}

	for i := 5; i < n; i++ {
		first := i - 1
		if rnd.Intn(2) == 0 {
			first = rnd.Intn(i)
		}
		list := []int{first}

		if rnd.Intn(3) != 0 {
			if second := rnd.Intn(i); second != first {
				list = append(list, second)
			}
		}
		parents[i] = list
	}

	return parents
}

// writeHistory creates the commits with git fast-import. Commit i has the parents parents[i], which
// must be lower indexes, and is also reachable as refs/heads/c<i>.
func writeHistory(t *testing.T, cfg config.Cfg, repoPath string, parents [][]int) []git.ObjectID {
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

// gitMergeBases returns the sorted output of git merge-base --all for a and b.
func gitMergeBases(t *testing.T, cfg config.Cfg, repoPath string, a, b git.ObjectID) []string {
	t.Helper()

	bases := strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "merge-base", "--all", a.String(), b.String())))
	sort.Strings(bases)
	return bases
}

// newAncestryEnv returns a repository with a local repo for it, and an object reader that reads
// it through the catfile cache, as landings do.
func newAncestryEnv(t *testing.T) (config.Cfg, string, *localrepo.Repo, catfile.ObjectContentReader) {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	// The repository is created without a server, which the ancestry checkers do not need.
	storagePath := cfg.Storages[0].Path
	repoProto := gittest.InitRepoDir(t, storagePath, gittest.NewRepositoryName(t))
	repoPath := filepath.Join(storagePath, repoProto.GetRelativePath())
	objectFormat := "sha1"
	if gittest.ObjectHashIsSHA256() {
		objectFormat = "sha256"
	}
	gittest.Exec(t, cfg, "init", "--bare", "--quiet", "--object-format="+objectFormat, repoPath)

	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)

	reader, cancel, err := catfileCache.ObjectReader(ctx, repo)
	require.NoError(t, err)
	t.Cleanup(cancel)

	return cfg, repoPath, repo, reader
}

// writeDatedHistory is writeHistory with the committer date dates[i] for commit i.
func writeDatedHistory(t *testing.T, cfg config.Cfg, repoPath string, parents [][]int, dates []int64) []git.ObjectID {
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

// skewedDates returns dates that grow with the index of the commit, except for random runs of
// commits that are dated a year before the first commit.
func skewedDates(rnd *rand.Rand, n int) []int64 {
	dates := make([]int64, n)
	for i := range dates {
		dates[i] = 1600000000 + int64(i)
	}
	for run := 0; run < n/16; run++ {
		start := rnd.Intn(n)
		for k := 0; k < 6+rnd.Intn(12) && start+k < n; k++ {
			dates[start+k] = 1568000000 + int64(k)
		}
	}
	return dates
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

// exactRangeOf returns the range base..head computed from the history itself, as rangeSet returns
// a range.
func exactRangeOf(parents [][]int, ids []git.ObjectID, base, head int) map[git.ObjectID][]git.ObjectID {
	baseAncestors := ancestorsOf(parents, base)
	set := map[git.ObjectID][]git.ObjectID{}
	for x := range ancestorsOf(parents, head) {
		if baseAncestors[x] {
			continue
		}
		var ps []git.ObjectID
		for _, p := range parents[x] {
			ps = append(ps, ids[p])
		}
		set[ids[x]] = ps
	}
	return set
}

// TestRangesMatchTheHistory checks, on seeded random histories with criss-crosses and an unrelated
// tree, with committer dates that grow along the history and with skewed ones, the ranges against
// the ones computed from the history itself: the listing of the graph checker and the exact
// ranges of both checkers equal them, and the listing of the git checker holds them and otherwise
// only commits that the base reaches.
func TestRangesMatchTheHistory(t *testing.T) {
	t.Parallel()

	for _, skewed := range []bool{false, true} {
		for _, seed := range []int64{1, 2} {
			t.Run(fmt.Sprintf("skewed %v seed %d", skewed, seed), func(t *testing.T) {
				t.Parallel()

				ctx := testhelper.Context(t)
				cfg, repoPath, repo, reader := newAncestryEnv(t)
				rnd := rand.New(rand.NewSource(seed))

				parents := crissCrossHistory(rnd, 220)
				for i := 0; i < 40; i++ {
					if i == 0 {
						parents = append(parents, nil)
						continue
					}
					parents = append(parents, []int{len(parents) - 1})
				}

				dates := make([]int64, len(parents))
				for i := range dates {
					dates[i] = 1600000000 + int64(i)
				}
				if skewed {
					dates = skewedDates(rnd, len(parents))
				}
				ids := writeDatedHistory(t, cfg, repoPath, parents, dates)
				require.Len(t, gitMergeBases(t, cfg, repoPath, ids[3], ids[4]), 2, "commits 3 and 4 must form a criss-cross")
				index := make(map[git.ObjectID]int, len(ids))
				for i, id := range ids {
					index[id] = i
				}

				store := derived.NewStore(repoPath)
				graphChecker, err := Graph(ctx, repo, reader, store)
				require.NoError(t, err)
				gitChecker := Git(repo)

				type pair struct{ base, head int }
				pairs := []pair{{3, 4}, {4, 3}, {3, 3}, {0, 219}, {219, len(ids) - 1}}
				for _, i := range []int{1, 50, 100, 150, 219} {
					pairs = append(pairs, pair{0, i}, pair{i, 0})
				}
				for range 150 {
					pairs = append(pairs, pair{rnd.Intn(len(ids)), rnd.Intn(len(ids))})
				}

				overListed, nonEmpty := 0, 0
				for _, p := range pairs {
					base, head := ids[p.base], ids[p.head]
					want := exactRangeOf(parents, ids, p.base, p.head)
					if len(want) > 0 {
						nonEmpty++
					}

					listed, err := graphChecker.List(ctx, head, []git.ObjectID{base})
					require.NoError(t, err)
					require.Equal(t, want, rangeSet(listed), "graph listing of %d..%d", p.base, p.head)

					listed, err = gitChecker.List(ctx, head, []git.ObjectID{base})
					require.NoError(t, err)
					baseAncestors := ancestorsOf(parents, p.base)
					extra := false
					for id, ps := range rangeSet(listed) {
						if wantParents, ok := want[id]; ok {
							require.Equal(t, wantParents, ps)
							continue
						}
						require.True(t, baseAncestors[index[id]], "git listing of %d..%d holds c%d, which base does not reach", p.base, p.head, index[id])
						extra = true
					}
					for id := range want {
						require.Contains(t, rangeSet(listed), id, "git listing of %d..%d misses c%d", p.base, p.head, index[id])
					}
					if extra {
						overListed++
					}

					for name, checker := range map[string]Checker{"git": gitChecker, "graph": graphChecker} {
						rng, err := revrange.Exact(ctx, checker, base, head)
						require.NoError(t, err)
						require.Equal(t, want, rangeSet(rng), "%s: exact range %d..%d", name, p.base, p.head)
					}
				}
				require.Positive(t, nonEmpty)

				// Skewed dates must produce listings that git rev-list gets wrong, which the test
				// is about; growing dates must not.
				if skewed {
					require.Positive(t, overListed)
				} else {
					require.Zero(t, overListed)
				}

				// The entries computed above are stored, so a checker that starts with an empty
				// in-memory cache gives the same answers from the store.
				_, stored, err := store.Kind(graphKindName, graphKindVersion).Get(ids[3])
				require.NoError(t, err)
				require.True(t, stored, "the graph entry of commit 3 is stored")

				fresh, err := Graph(ctx, repo, reader, store)
				require.NoError(t, err)
				for _, p := range pairs[:60] {
					listed, err := fresh.List(ctx, ids[p.head], []git.ObjectID{ids[p.base]})
					require.NoError(t, err)
					require.Equal(t, exactRangeOf(parents, ids, p.base, p.head), rangeSet(listed))
				}
			})
		}
	}
}

// TestSkewedChainOfTheReview is the history that the review of phase 4b reported: a root R dated
// 2026-01, A on it dated 2026-02, and ten commits on A dated 2025-06. git rev-list tip..A lists A
// and R although tip reaches both. A is already on the branch, so its exact range is empty, and a
// commit C on A has the exact range {C}.
func TestSkewedChainOfTheReview(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, repoPath, repo, reader := newAncestryEnv(t)

	parents := [][]int{nil, {0}}
	dates := []int64{1767225600, 1769904000} // 2026-01-01, 2026-02-01
	for i := 0; i < 10; i++ {
		parents = append(parents, []int{len(parents) - 1})
		dates = append(dates, 1748736000+int64(i)) // 2025-06-01
	}
	parents = append(parents, []int{1}) // C on A
	dates = append(dates, 1769904100)
	ids := writeDatedHistory(t, cfg, repoPath, parents, dates)
	root, a, tip, c := ids[0], ids[1], ids[11], ids[12]

	listed := strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list", tip.String()+".."+a.String())))
	require.ElementsMatch(t, []string{a.String(), root.String()}, listed, "git rev-list no longer shows the skew this test is about")

	graphChecker, err := Graph(ctx, repo, reader, derived.NewStore(repoPath))
	require.NoError(t, err)

	for name, checker := range map[string]Checker{"git": Git(repo), "graph": graphChecker} {
		rng, err := revrange.Exact(ctx, checker, tip, a)
		require.NoError(t, err)
		require.Empty(t, rng, "%s: A is reachable from tip", name)

		rng, err = revrange.Exact(ctx, checker, tip, c)
		require.NoError(t, err)
		require.Equal(t, map[git.ObjectID][]git.ObjectID{c: {a}}, rangeSet(rng), "%s: range of C", name)
	}
}

func TestRangeWithoutCommonAncestor(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, repoPath, repo, reader := newAncestryEnv(t)

	// Two root commits share no history.
	a := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("a"))
	b := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("b"))

	graphChecker, err := Graph(ctx, repo, reader, derived.NewStore(repoPath))
	require.NoError(t, err)

	for name, checker := range map[string]Checker{"git": Git(repo), "graph": graphChecker} {
		t.Run(name, func(t *testing.T) {
			// Nothing of b's history is reachable from a, so the range is b down to its root.
			rng, err := revrange.Exact(ctx, checker, a, b)
			require.NoError(t, err)
			require.Equal(t, map[git.ObjectID][]git.ObjectID{b: nil}, rangeSet(rng))

			bases, err := checker.MergeBases(ctx, a, b)
			require.NoError(t, err)
			require.Empty(t, bases)
		})
	}
}

func TestNewChoosesCheckerAndFallsBack(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	logger := testhelper.NewLogger(t)

	isGraph := func(checker Checker) bool {
		_, ok := checker.(graphChecker)
		return ok
	}

	t.Run("disabled uses git", func(t *testing.T) {
		_, repoPath, repo, reader := newAncestryEnv(t)

		require.False(t, isGraph(New(ctx, false, repo, reader, derived.NewStore(repoPath), logger)))
	})

	t.Run("enabled uses graph", func(t *testing.T) {
		_, repoPath, repo, reader := newAncestryEnv(t)

		require.True(t, isGraph(New(ctx, true, repo, reader, derived.NewStore(repoPath), logger)))
	})

	t.Run("shallow repository falls back to git", func(t *testing.T) {
		_, repoPath, repo, reader := newAncestryEnv(t)
		require.NoError(t, os.WriteFile(filepath.Join(repoPath, "shallow"), nil, 0o600))

		require.False(t, isGraph(New(ctx, true, repo, reader, derived.NewStore(repoPath), logger)))
	})

	t.Run("replace refs fall back to git", func(t *testing.T) {
		cfg, repoPath, repo, reader := newAncestryEnv(t)
		first := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("first"))
		second := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("second"))
		gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/replace/"+first.String(), second.String())

		_, err := Graph(ctx, repo, reader, derived.NewStore(repoPath))
		require.Error(t, err)

		require.False(t, isGraph(New(ctx, true, repo, reader, derived.NewStore(repoPath), logger)))
	})

	t.Run("missing repository or reader is an error", func(t *testing.T) {
		_, err := Graph(ctx, nil, nil, nil)
		require.Error(t, err)
	})
}

// rangeSet returns the range as a map from commit to its parents, which does not depend on the
// order of the listing. A root commit maps to nil.
func rangeSet(commits []RangeCommit) map[git.ObjectID][]git.ObjectID {
	set := make(map[git.ObjectID][]git.ObjectID, len(commits))
	for _, c := range commits {
		var parents []git.ObjectID
		if len(c.Parents) > 0 {
			parents = c.Parents
		}
		set[c.ID] = parents
	}
	return set
}
