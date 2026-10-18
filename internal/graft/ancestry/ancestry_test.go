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

func TestGraphMatchesGitOnRandomDAG(t *testing.T) {
	t.Parallel()

	for _, seed := range []int64{1, 2} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg, repoPath, repo, reader := newAncestryEnv(t)
			rnd := rand.New(rand.NewSource(seed))

			ids := writeHistory(t, cfg, repoPath, crissCrossHistory(rnd, 220))
			require.Len(t, gitMergeBases(t, cfg, repoPath, ids[3], ids[4]), 2, "commits 3 and 4 must form a criss-cross")

			store := derived.NewStore(repoPath)
			graphChecker, err := Graph(ctx, repo, reader, store)
			require.NoError(t, err)
			gitChecker := Git(repo)

			// Pairs: the criss-cross, self pairs, the root against every commit, and random pairs.
			type pair struct{ a, b git.ObjectID }
			var pairs []pair
			pairs = append(pairs, pair{ids[3], ids[4]}, pair{ids[4], ids[3]}, pair{ids[3], ids[3]})
			for _, id := range ids[1:] {
				pairs = append(pairs, pair{ids[0], id}, pair{id, ids[0]})
			}
			for range 150 {
				pairs = append(pairs, pair{ids[rnd.Intn(len(ids))], ids[rnd.Intn(len(ids))]})
			}

			requireAgree := func(checker Checker, label string, pairs []pair) {
				t.Helper()

				for _, p := range pairs {
					want, err := gitChecker.Range(ctx, p.a, p.b)
					require.NoError(t, err)
					got, err := checker.Range(ctx, p.a, p.b)
					require.NoError(t, err)
					require.Equal(t, rangeSet(want), rangeSet(got), "%s: Range(%s, %s)", label, p.a, p.b)
				}
			}

			requireAgree(graphChecker, "graph", pairs)

			// The entries computed above are stored, so a checker that starts with an empty
			// in-memory cache gives the same answers from the store.
			_, stored, err := store.Kind(graphKindName, graphKindVersion).Get(ids[3])
			require.NoError(t, err)
			require.True(t, stored, "the graph entry of commit 3 is stored")

			fresh, err := Graph(ctx, repo, reader, store)
			require.NoError(t, err)
			requireAgree(fresh, "graph from store", pairs[:60])
		})
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
			rng, err := checker.Range(ctx, a, b)
			require.NoError(t, err)
			require.Equal(t, map[git.ObjectID][]git.ObjectID{b: nil}, rangeSet(rng))
		})
	}
}

func TestNewChoosesCheckerAndFallsBack(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	logger := testhelper.NewLogger(t)

	t.Run("disabled uses git", func(t *testing.T) {
		_, repoPath, repo, reader := newAncestryEnv(t)

		_, isGit := New(ctx, false, repo, reader, derived.NewStore(repoPath), logger).(gitChecker)
		require.True(t, isGit)
	})

	t.Run("enabled uses graph", func(t *testing.T) {
		_, repoPath, repo, reader := newAncestryEnv(t)

		_, isGraph := New(ctx, true, repo, reader, derived.NewStore(repoPath), logger).(graphChecker)
		require.True(t, isGraph)
	})

	t.Run("shallow repository falls back to git", func(t *testing.T) {
		_, repoPath, repo, reader := newAncestryEnv(t)
		require.NoError(t, os.WriteFile(filepath.Join(repoPath, "shallow"), nil, 0o600))

		_, isGit := New(ctx, true, repo, reader, derived.NewStore(repoPath), logger).(gitChecker)
		require.True(t, isGit)
	})

	t.Run("replace refs fall back to git", func(t *testing.T) {
		cfg, repoPath, repo, reader := newAncestryEnv(t)
		first := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("first"))
		second := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("second"))
		gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/replace/"+first.String(), second.String())

		_, err := Graph(ctx, repo, reader, derived.NewStore(repoPath))
		require.Error(t, err)

		_, isGit := New(ctx, true, repo, reader, derived.NewStore(repoPath), logger).(gitChecker)
		require.True(t, isGit)
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

func TestRangeGraphMatchesGit(t *testing.T) {
	t.Parallel()

	for _, seed := range []int64{1, 2} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg, repoPath, repo, reader := newAncestryEnv(t)
			rnd := rand.New(rand.NewSource(seed))

			// A criss-cross history with a second, unrelated tree on top of it.
			parents := crissCrossHistory(rnd, 160)
			for i := 0; i < 40; i++ {
				if i == 0 {
					parents = append(parents, nil)
					continue
				}
				parents = append(parents, []int{len(parents) - 1})
			}
			ids := writeHistory(t, cfg, repoPath, parents)

			graphChecker, err := Graph(ctx, repo, reader, derived.NewStore(repoPath))
			require.NoError(t, err)
			gitChecker := Git(repo)

			type pair struct{ base, head git.ObjectID }
			pairs := []pair{{ids[3], ids[4]}, {ids[4], ids[3]}, {ids[3], ids[3]}, {ids[0], ids[159]}, {ids[159], ids[len(ids)-1]}}
			for range 150 {
				pairs = append(pairs, pair{ids[rnd.Intn(len(ids))], ids[rnd.Intn(len(ids))]})
			}

			nonEmpty := 0
			for _, p := range pairs {
				want, err := gitChecker.Range(ctx, p.base, p.head)
				require.NoError(t, err)
				got, err := graphChecker.Range(ctx, p.base, p.head)
				require.NoError(t, err)
				require.Equal(t, rangeSet(want), rangeSet(got), "Range(%s, %s)", p.base, p.head)

				// The git checker lists what git rev-list lists.
				listed := strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list", p.base.String()+".."+p.head.String())))
				require.Len(t, want, len(listed))

				// A range is empty exactly when head is reachable from base.
				reachable := gittest.NewCommand(t, cfg, "-C", repoPath, "merge-base", "--is-ancestor", p.head.String(), p.base.String()).Run() == nil
				require.Equal(t, reachable, len(want) == 0)
				if len(want) > 0 {
					nonEmpty++
				}
			}
			require.Positive(t, nonEmpty)
		})
	}
}
