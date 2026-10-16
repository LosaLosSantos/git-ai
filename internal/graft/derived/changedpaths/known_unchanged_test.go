package changedpaths

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestIndex_KnownUnchanged(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newFakeObjects()
	r, c2, c3, c4, _ := chain(f)

	store := newMemStore()
	index := NewIndex(f, store)
	for _, commit := range []string{r, c2, c3} {
		_, err := index.CommitSummary(ctx, commit)
		require.NoError(t, err)
	}
	reads := f.treeReads
	computed := index.Computed()

	known := func(commit, path string) bool {
		t.Helper()
		unchanged, err := index.KnownUnchanged(ctx, git.ObjectID(commit), path)
		require.NoError(t, err)
		return unchanged
	}

	// c3 adds b and leaves a/x alone.
	require.True(t, known(c3, "a/x"))
	require.True(t, known(c3, "a"))
	require.True(t, known(c3, "missing"))
	require.False(t, known(c3, "b"))
	require.False(t, known(c3, "."))
	// c2 changes a/x.
	require.False(t, known(c2, "a/x"))
	require.False(t, known(c2, "a"))
	require.True(t, known(c2, "b"))
	// The root commit adds a/x.
	require.False(t, known(r, "a/x"))
	require.True(t, known(r, "b"))
	// Paths are normalized as the Deriver normalizes them.
	require.True(t, known(c3, "./a/x"))
	require.False(t, known(c3, "a/../b"))

	// A commit without a stored summary is not known to be unchanged, and its summary
	// is not computed: the filter reads the store only.
	require.False(t, known(c4, "a/x"))
	require.Equal(t, reads, f.treeReads)
	require.Equal(t, computed, index.Computed())
	_, stored := store.data[c4]
	require.False(t, stored)

	// Paths that PathHistory does not answer are never known to be unchanged.
	require.False(t, known(c3, "../a"))
	require.False(t, known(c3, "/a"))

	// Neither are corrupt entries and overflows.
	store.data[c3] = []byte{9, 9, 9}
	require.False(t, known(c3, "a/x"))
	store.data[c3] = Summary{Overflow: true}.encode()
	require.False(t, known(c3, "a/x"))

	// Without a store nothing is known.
	unchanged, err := NewIndex(f, nil).KnownUnchanged(ctx, git.ObjectID(c3), "a/x")
	require.NoError(t, err)
	require.False(t, unchanged)
}

// TestIndex_PathHistoryComputesWithinTheWriteBudget requires a cold query to stop
// computing whole-tree summaries once it cannot store them any more, and to diff the
// path alone for the remaining commits.
func TestIndex_PathHistoryComputesWithinTheWriteBudget(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	const length = maxStoredPerQuery + 200
	var tip string
	var changes []string
	for i := 0; i < length; i++ {
		// a/x changes every 100 commits, and b in every commit.
		tree := f.tree(subtree("a", f.tree(blob("x", fmt.Sprint(i/100)))), blob("b", fmt.Sprint(i)))
		if tip == "" {
			tip = f.commit(tree)
		} else {
			tip = f.commit(tree, tip)
		}
		if i%100 == 0 {
			changes = append([]string{tip}, changes...)
		}
	}

	want := collect(t, NewIndex(f, nil), tip, "a/x", unlimited)
	require.Equal(t, changes, want)

	store := newMemStore()
	index := NewIndex(f, store)
	require.Equal(t, want, collect(t, index, tip, "a/x", unlimited))
	require.Equal(t, int64(maxStoredPerQuery), index.Computed())
	require.Len(t, store.data, maxStoredPerQuery)

	// The next query stores the summaries that the first one could not.
	require.Equal(t, want, collect(t, index, tip, "a/x", unlimited))
	require.Equal(t, int64(length), index.Computed())
	require.Len(t, store.data, length)
}

// filteredHistory returns the commits that the Deriver reports with the summaries of
// index as the pre-filter.
func filteredHistory(ctx context.Context, tb testing.TB, deriver *derived.Deriver, index *Index, start git.ObjectID, path string, limit int) []string {
	tb.Helper()

	var ids []string
	require.NoError(tb, deriver.PathHistoryFiltered(ctx, start, path, limit, index.KnownUnchanged, func(commit *catfile.Commit) error {
		ids = append(ids, commit.GetId())
		return nil
	}))
	return ids
}

// TestKnownUnchanged_FilteredDeriverMatchesPathHistory requires the Deriver with the
// changed-paths pre-filter, which DerivedDataService.PathHistory uses, to report
// exactly what the Deriver reports without it, whether no summary, some or all of
// them are stored.
func TestKnownUnchanged_FilteredDeriverMatchesPathHistory(t *testing.T) {
	t.Parallel()

	histories := map[string][]fiCommit{
		"linear":    linearHistory,
		"merge":     mergeHistory,
		"rename":    renameHistory,
		"directory": directoryHistory,
	}

	for name, commits := range histories {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			repoProto, repoPath := newBareRepo(t, cfg)
			repo := localrepo.NewTestRepo(t, cfg, repoProto)
			ids := importHistory(t, cfg, repoPath, commits)

			reader := newObjectReader(t, ctx, cfg, repo)
			logger := testhelper.NewLogger(t)
			reference, err := derived.NewDeriver(ctx, logger, repo, reader, false)
			require.NoError(t, err)

			for _, warmed := range []string{"none", "half", "all"} {
				summaries := derived.NewStore(testhelper.TempDir(t))
				var warm []git.ObjectID
				for i, commit := range commits {
					if warmed == "all" || (warmed == "half" && i%2 == 0) {
						warm = append(warm, ids[commit.name])
					}
				}
				require.NoError(t, Warm(ctx, derived.WarmEnv{Logger: logger, Repo: repo, Reader: reader, Store: summaries}, warm))
				index := NewIndexFor(reader, summaries)

				for _, cache := range []bool{false, true} {
					deriver, err := derived.NewDeriver(ctx, logger, repo, reader, cache)
					require.NoError(t, err)

					for _, commit := range commits {
						for _, path := range equivalencePaths {
							for _, limit := range []int{1000, 2} {
								require.Equal(t,
									derivedHistory(ctx, t, reference, ids[commit.name], path, limit),
									filteredHistory(ctx, t, deriver, index, ids[commit.name], path, limit),
									"%s summaries, cache %v: %q from %s, limit %d", warmed, cache, path, commit.name, limit)
							}
						}
					}
				}

				// The filter never computes a summary.
				require.Zero(t, index.Computed())
			}
		})
	}
}

// pathHistoryRepro builds the history of the PathHistory performance report: a
// linear history in which a/x changes every 500 commits, and every other commit
// changes one of many files elsewhere.
func pathHistoryRepro(tb testing.TB, length int) ([]fiCommit, int) {
	tb.Helper()

	commits := make([]fiCommit, 0, length)
	changes := 0
	for i := 0; i < length; i++ {
		commit := fiCommit{name: fmt.Sprintf("c%d", i)}
		if i > 0 {
			commit.parents = []string{fmt.Sprintf("c%d", i-1)}
		}
		switch {
		case i%500 == 0:
			commit.ops = []fiOp{put("a/x", fmt.Sprint(i))}
			changes++
		case i%2 == 0:
			commit.ops = []fiOp{put(fmt.Sprintf("p/q/r/s%d/y%d", i%7, i%11), fmt.Sprint(i))}
		default:
			commit.ops = []fiOp{put("b/y", fmt.Sprint(i))}
		}
		commits = append(commits, commit)
	}
	return commits, changes
}

// TestKnownUnchanged_FilteredDeriverCost is the regression test of the PathHistory
// performance report: the service answered from changed-paths summaries that it
// computed itself, diffing the whole tree of every commit of the history on a cold
// query and reading every commit of the history on every later one. With the
// summaries as a pre-filter of the Deriver's walk, a cold query reads no more trees
// than the Deriver alone, computes no summary, and a repeated query reads no tree.
func TestKnownUnchanged_FilteredDeriverCost(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := newBareRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	const length = 1500
	commits, changes := pathHistoryRepro(t, length)
	ids := importHistory(t, cfg, repoPath, commits)
	tip := ids[fmt.Sprintf("c%d", length-1)]

	reader := newObjectReader(t, ctx, cfg, repo)
	logger := testhelper.NewLogger(t)

	reference, err := derived.NewDeriver(ctx, logger, repo, reader, false)
	require.NoError(t, err)
	want := derivedHistory(ctx, t, reference, tip, "a/x", 100)
	require.Len(t, want, changes)

	var trees atomic.Int64
	counting := countingReader{ObjectContentReader: reader, trees: &trees}
	query := func() ([]string, int64) {
		t.Helper()
		trees.Store(0)
		// The service builds a new Deriver and index for every request.
		deriver, err := derived.NewDeriver(ctx, logger, repo, counting, true)
		require.NoError(t, err)
		index := NewIndexFor(counting, derived.NewStore(repoPath))
		got := filteredHistory(ctx, t, deriver, index, tip, "a/x", 100)
		require.Zero(t, index.Computed())
		return got, trees.Load()
	}

	// Without stored summaries the walk is the Deriver's: one tree per commit.
	got, cold := query()
	require.Equal(t, want, got)
	require.LessOrEqual(t, cold, int64(length+changes))
	_, err = os.Stat(filepath.Join(repoPath, "graft", "derived", kindName))
	require.ErrorIs(t, err, os.ErrNotExist, "a query must not store summaries")

	// A repeated query is answered by the stored chain values.
	got, warm := query()
	require.Equal(t, want, got)
	require.Zero(t, warm)

	// With the summaries stored by the eager warmer, a cold query reads only the trees
	// of the commits that changed the path.
	require.NoError(t, os.RemoveAll(filepath.Join(repoPath, "graft")))
	all := make([]git.ObjectID, 0, length)
	for _, commit := range commits {
		all = append(all, ids[commit.name])
	}
	require.NoError(t, Warm(ctx, derived.WarmEnv{Logger: logger, Repo: repo, Reader: reader, Store: derived.NewStore(repoPath)}, all))

	got, filtered := query()
	require.Equal(t, want, got)
	require.LessOrEqual(t, filtered, int64(2*changes))
}
