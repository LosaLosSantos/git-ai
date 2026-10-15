package derived

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// mergeHistory is a history with linear, merge, delete, re-add, rename and mode
// change cases. Its merges are constructed so that the path is TREESAME to the
// second parent (merge3), to the first parent (merge1's b), to neither (merge2)
// and to both (merge1's a is equal to side1).
func mergeHistory() []historyCommit {
	return []historyCommit{
		{name: "root", changes: []fileChange{
			{path: "a", content: "1"},
			{path: "b", content: "1"},
			{path: "dir/x", content: "1"},
		}},
		{name: "side1", parents: []string{"root"}, changes: []fileChange{{path: "a", content: "2"}}},
		{name: "main1", parents: []string{"root"}, changes: []fileChange{{path: "b", content: "2"}}},
		// Merge that takes side1's version of a: TREESAME to the second parent only.
		{name: "merge1", parents: []string{"main1", "side1"}, changes: []fileChange{{path: "a", content: "2"}}},
		{name: "main2", parents: []string{"merge1"}, changes: []fileChange{{path: "dir/x", content: "2"}}},
		{name: "side2", parents: []string{"side1"}, changes: []fileChange{{path: "a", content: "3"}}},
		// Merge that resolves a to a new version: TREESAME to neither parent.
		{name: "merge2", parents: []string{"main2", "side2"}, changes: []fileChange{{path: "a", content: "4"}}},
		{name: "main3", parents: []string{"merge2"}, changes: []fileChange{{path: "b", delete: true}}},
		{name: "main4", parents: []string{"main3"}, changes: []fileChange{{path: "b", content: "back"}}},
		// Merge whose a equals side2's: TREESAME to the second parent, not the first.
		{name: "merge3", parents: []string{"main4", "side2"}, changes: []fileChange{{path: "a", content: "3"}}},
		{name: "main5", parents: []string{"merge3"}, changes: []fileChange{
			{path: "a", delete: true},
			{path: "renamed", content: "3"},
		}},
		{name: "main6", parents: []string{"main5"}, changes: []fileChange{{path: "renamed", mode: "100755", content: "3"}}},
		{name: "main7", parents: []string{"main6"}, changes: []fileChange{{path: "dir/x", delete: true}}},
	}
}

var mergeHistoryPaths = []string{"a", "b", "dir", "dir/x", "renamed", "missing", ".", "dir/missing"}

func TestLastMod_MatchesGitLogLinear(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	commits := linearHistory(8, func(i int) string {
		switch i {
		case 0, 2, 5:
			return "a"
		case 1:
			return "b"
		case 3:
			return "dir/x"
		case 6:
			return "dir/y"
		}
		return ""
	})
	ids := writeHistory(t, cfg, repoPath, commits)

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	requireMatchesGitLastMod(t, ctx, cfg, repoPath, deriver, ids, namesOf(commits), []string{"a", "b", "dir", "dir/x", "dir/y", ".", "missing"})
}

func TestLastMod_MatchesGitLogMerges(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	commits := mergeHistory()
	ids := writeHistory(t, cfg, repoPath, commits)

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	requireMatchesGitLastMod(t, ctx, cfg, repoPath, deriver, ids, namesOf(commits), mergeHistoryPaths)

	// The same answers must come out when nothing is read from the store.
	uncached := newTestDeriver(t, ctx, cfg, repo, false)
	requireMatchesGitLastMod(t, ctx, cfg, repoPath, uncached, ids, namesOf(commits), mergeHistoryPaths)
}

func TestLastMod_SpecificValues(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, mergeHistory())
	deriver := newTestDeriver(t, ctx, cfg, repo, true)

	for _, tc := range []struct {
		desc     string
		commit   string
		path     string
		expected string
	}{
		{desc: "root with path", commit: "root", path: "a", expected: "root"},
		{desc: "root without path", commit: "root", path: "missing", expected: ""},
		{desc: "unchanged since root", commit: "main1", path: "a", expected: "root"},
		{desc: "merge TREESAME to second parent", commit: "merge3", path: "a", expected: "side2"},
		{desc: "merge TREESAME to first parent", commit: "merge1", path: "b", expected: "main1"},
		{desc: "merge changed relative to both parents", commit: "merge2", path: "a", expected: "merge2"},
		{desc: "deletion is a change", commit: "main3", path: "b", expected: "main3"},
		{desc: "re-add after deletion", commit: "main4", path: "b", expected: "main4"},
		{desc: "rename deletes old path", commit: "main5", path: "a", expected: "main5"},
		{desc: "rename adds new path", commit: "main5", path: "renamed", expected: "main5"},
		{desc: "mode change is a change", commit: "main6", path: "renamed", expected: "main6"},
		{desc: "unchanged through merges", commit: "main6", path: "dir/x", expected: "main2"},
		{desc: "directory deleted", commit: "main7", path: "dir", expected: "main7"},
		{desc: "root directory", commit: "main7", path: ".", expected: "main7"},
		{desc: "empty path is root", commit: "main6", path: "", expected: "main6"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := deriver.LastMod(ctx, ids[tc.commit], tc.path)
			require.NoError(t, err)

			if tc.expected == "" {
				require.Empty(t, got)
				return
			}
			require.Equal(t, ids[tc.expected], got)
		})
	}
}

func TestLastMod_UnknownCommit(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, _ := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	_, err := deriver.LastMod(ctx, git.ObjectID("0000000000000000000000000000000000000001"), "a")
	require.Error(t, err)
}

func TestLastMod_AbsentPathIsNotStored(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, linearHistory(3, func(i int) string { return "" }))

	// Every path name a client invents is absent, so storing absent answers would let
	// readers create store entries without bound.
	for i := 0; i < 3; i++ {
		deriver := newTestDeriver(t, ctx, cfg, repo, true)
		got, err := deriver.LastMod(ctx, ids["c2"], fmt.Sprintf("missing-%d", i))
		require.NoError(t, err)
		require.Empty(t, got)
	}
	require.NoDirExists(t, filepath.Join(repoPath, "graft"))

	// An absent answer stored by an earlier version is still used. It is an explicit
	// value, not an empty file.
	require.NoError(t, NewStore(repoPath).Put(kindLastMod, Key(kindLastMod, ids["c2"], "a"), []byte(absentValue)))

	cached := newTestDeriver(t, ctx, cfg, repo, true)
	got, err := cached.LastMod(ctx, ids["c2"], "a")
	require.NoError(t, err)
	require.Empty(t, got)
	require.Zero(t, cached.CommitsComputed(), "a stored absent answer must be used")
}

func TestLastMod_CorruptEntriesAreRepaired(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, linearHistory(4, func(i int) string {
		if i == 1 {
			return "a"
		}
		return ""
	}))

	// A truncated entry, as left behind by a crash, and an entry with garbage must
	// neither be read as an answer nor stay on disk.
	store := NewStore(repoPath)
	truncated := Key(kindLastMod, ids["c3"], "a")
	garbage := Key(kindLastMod, ids["c2"], "a")
	require.NoError(t, store.Put(kindLastMod, truncated, nil))
	require.NoError(t, store.Put(kindLastMod, garbage, []byte("garbage")))

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	got, err := deriver.LastMod(ctx, ids["c3"], "a")
	require.NoError(t, err)
	require.Equal(t, ids["c1"], got)

	for _, key := range []string{truncated, garbage} {
		value, found, err := store.Get(kindLastMod, key)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, ids["c1"].String(), string(value), "the corrupt entry must be repaired")
	}
}

func TestPathHistory_LimitDoesNotWalkBeyondIt(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, linearHistory(100, func(i int) string {
		if i == 0 || i == 10 {
			return "a"
		}
		return ""
	}))

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	var got []string
	require.NoError(t, deriver.PathHistory(ctx, ids["c99"], "a", 1, func(commit *catfile.Commit) error {
		got = append(got, commit.GetId())
		return nil
	}))
	require.Equal(t, []string{ids["c10"].String()}, got)
	require.Equal(t, int64(90), deriver.CommitsComputed(), "only the walk from c99 down to c10 may run")
}

func TestLastMod_StoresCheckpointsOnTheWay(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	const length = 3*checkpointInterval + 10
	commits := linearHistory(length, func(i int) string {
		if i == 1 {
			return "a"
		}
		return ""
	})
	ids := writeHistory(t, cfg, repoPath, commits)
	head := length - 1

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	got, err := deriver.LastMod(ctx, ids[fmt.Sprintf("c%d", head)], "a")
	require.NoError(t, err)
	require.Equal(t, ids["c1"], got)

	// The chain is c<head> down to c1. Its first member, every checkpointInterval-th
	// member and the result c1 are stored, and nothing else.
	store := NewStore(repoPath)
	var stored []int
	for i := 0; i < length; i++ {
		value, found, err := store.Get(kindLastMod, Key(kindLastMod, ids[fmt.Sprintf("c%d", i)], "a"))
		require.NoError(t, err)
		if found {
			require.Equal(t, ids["c1"].String(), string(value))
			stored = append(stored, i)
		}
	}
	require.Equal(t, []int{1, head - 3*checkpointInterval, head - 2*checkpointInterval, head - checkpointInterval, head}, stored)
	require.Equal(t, len(stored), countStoreEntries(t, repoPath))

	// A query that starts anywhere on the chain reaches a stored member within the interval.
	for _, start := range []int{head - 1, head - checkpointInterval - 1, 2} {
		later := newTestDeriver(t, ctx, cfg, repo, true)
		got, err := later.LastMod(ctx, ids[fmt.Sprintf("c%d", start)], "a")
		require.NoError(t, err)
		require.Equal(t, ids["c1"], got)
		require.Less(t, later.CommitsComputed(), int64(checkpointInterval), "query at c%d", start)
	}
}

// countStoreEntries returns the number of entries of the derived-data store of the
// repository, including temporary files.
func countStoreEntries(tb testing.TB, repoPath string) int {
	tb.Helper()

	count := 0
	root := filepath.Join(repoPath, "graft", "derived")
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return 0
	}
	require.NoError(tb, filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	}))
	return count
}

func TestLastMod_CacheHitComputesNothing(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, mergeHistory())

	first := newTestDeriver(t, ctx, cfg, repo, true)
	want, err := first.LastMod(ctx, ids["main7"], "a")
	require.NoError(t, err)
	require.Equal(t, ids["main5"], want)
	require.Equal(t, int64(3), first.CommitsComputed(), "main7, main6 and main5 are walked")

	second := newTestDeriver(t, ctx, cfg, repo, true)
	got, err := second.LastMod(ctx, ids["main7"], "a")
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Zero(t, second.CommitsComputed(), "a cached query must not compute any commit")

	// An ancestor on a stored chain reaches a stored member: main6 is not a checkpoint of
	// the chain main7, main6, main5, but its next commit main5 is.
	third := newTestDeriver(t, ctx, cfg, repo, true)
	got, err = third.LastMod(ctx, ids["main6"], "a")
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, int64(1), third.CommitsComputed())

	// That query stored its own start, so it is answered from the store now.
	fourth := newTestDeriver(t, ctx, cfg, repo, true)
	got, err = fourth.LastMod(ctx, ids["main6"], "a")
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Zero(t, fourth.CommitsComputed())
}

func TestLastMod_DisabledCacheWritesNothing(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, mergeHistory())

	deriver := newTestDeriver(t, ctx, cfg, repo, false)
	_, err := deriver.LastMod(ctx, ids["main7"], "a")
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(repoPath, "graft"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestLastMod_LongLinearHistoryWithoutRecursion(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	const length = 2000
	commits := linearHistory(length, func(i int) string {
		if i == 10 {
			return "a"
		}
		return ""
	})
	ids := writeHistory(t, cfg, repoPath, commits)
	head := ids[fmt.Sprintf("c%d", length-1)]

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	got, err := deriver.LastMod(ctx, head, "a")
	require.NoError(t, err)
	require.Equal(t, ids["c10"], got)
	require.Equal(t, gitLastCommitForPath(t, cfg, repoPath, head, "a"), got.String())
	require.Equal(t, int64(length-10), deriver.CommitsComputed())

	// The walk wrote checkpoints, not one entry per commit.
	require.LessOrEqual(t, countStoreEntries(t, repoPath), (length-10)/checkpointInterval+2)

	// Later queries at any depth reach a checkpoint after a short walk.
	later := newTestDeriver(t, ctx, cfg, repo, true)
	got, err = later.LastMod(ctx, ids["c1500"], "a")
	require.NoError(t, err)
	require.Equal(t, ids["c10"], got)
	require.Less(t, later.CommitsComputed(), int64(checkpointInterval))
}

func TestLastMod_ConcurrentReadersShareStore(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	commits := mergeHistory()
	ids := writeHistory(t, cfg, repoPath, commits)

	const workers = 8
	derivers := make([]*Deriver, workers)
	for i := range derivers {
		derivers[i] = newTestDeriver(t, ctx, cfg, repo, true)
	}

	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for _, deriver := range derivers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, name := range namesOf(commits) {
				for _, path := range mergeHistoryPaths {
					if _, err := deriver.LastMod(ctx, ids[name], path); err != nil {
						errs <- err
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	requireMatchesGitLastMod(t, ctx, cfg, repoPath, newTestDeriver(t, ctx, cfg, repo, true), ids, namesOf(commits), mergeHistoryPaths)
}

func namesOf(commits []historyCommit) []string {
	names := make([]string, 0, len(commits))
	for _, commit := range commits {
		names = append(names, commit.name)
	}
	return names
}

func requireMatchesGitLastMod(
	tb testing.TB,
	ctx context.Context,
	cfg config.Cfg,
	repoPath string,
	deriver *Deriver,
	ids map[string]git.ObjectID,
	names []string,
	paths []string,
) {
	tb.Helper()

	for _, name := range names {
		for _, path := range paths {
			want := gitLastCommitForPath(tb, cfg, repoPath, ids[name], path)

			got, err := deriver.LastMod(ctx, ids[name], path)
			require.NoError(tb, err)
			require.Equal(tb, want, got.String(), "lastmod of %q at %s", path, name)
		}
	}
}
