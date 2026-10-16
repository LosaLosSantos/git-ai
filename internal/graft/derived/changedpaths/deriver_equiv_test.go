package changedpaths

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// fiOp is a file operation of a test commit: a write of content to path, or the
// removal of path (a file or a whole directory).
type fiOp struct {
	path    string
	content string
	remove  bool
}

func put(path, content string) fiOp { return fiOp{path: path, content: content} }
func rm(path string) fiOp           { return fiOp{path: path, remove: true} }

// fiCommit is a commit of a test history. The tree of a commit is the tree of its
// first parent with ops applied, as git fast-import builds it.
type fiCommit struct {
	name    string
	parents []string
	ops     []fiOp
}

// linearHistory has deletes, a re-added file, a directory that is removed and a
// commit that changes nothing.
var linearHistory = []fiCommit{
	{name: "l0", ops: []fiOp{put("a/x", "1"), put("b", "1"), put("dir/y", "1")}},
	{name: "l1", parents: []string{"l0"}, ops: []fiOp{put("b", "2")}},
	{name: "l2", parents: []string{"l1"}, ops: []fiOp{put("a/x", "2")}},
	{name: "l3", parents: []string{"l2"}, ops: []fiOp{rm("a/x")}},
	{name: "l4", parents: []string{"l3"}, ops: []fiOp{put("a/x", "2")}},
	{name: "l5", parents: []string{"l4"}, ops: []fiOp{put("dir/z", "1")}},
	{name: "l6", parents: []string{"l5"}, ops: []fiOp{put("dir/y", "2"), put("dir/sub/w", "1")}},
	{name: "l7", parents: []string{"l6"}},
	{name: "l8", parents: []string{"l7"}, ops: []fiOp{rm("dir")}},
	{name: "l9", parents: []string{"l8"}, ops: []fiOp{put("dir/y", "3")}},
}

// mergeHistory has merges whose first parent differs from the merge in the path, and
// merges that equal their first parent.
var mergeHistory = []fiCommit{
	{name: "m0", ops: []fiOp{put("a/x", "1"), put("b", "1")}},
	{name: "m1", parents: []string{"m0"}, ops: []fiOp{put("a/x", "2")}},
	{name: "s1", parents: []string{"m0"}, ops: []fiOp{put("b", "2")}},
	{name: "s2", parents: []string{"s1"}, ops: []fiOp{put("a/x", "3")}},
	{name: "mg", parents: []string{"m1", "s2"}, ops: []fiOp{put("a/x", "3"), put("b", "2")}},
	{name: "mg2", parents: []string{"mg", "s2"}},
	{name: "mg3", parents: []string{"s2", "m1"}, ops: []fiOp{put("a/x", "4")}},
	{name: "mg4", parents: []string{"m1", "mg3"}},
	{name: "mg5", parents: []string{"mg4"}, ops: []fiOp{rm("a/x")}},
}

// renameHistory moves files and directories, which git log -- path does not follow.
var renameHistory = []fiCommit{
	{name: "r0", ops: []fiOp{put("old", "1"), put("keep", "1")}},
	{name: "r1", parents: []string{"r0"}, ops: []fiOp{rm("old"), put("new", "1")}},
	{name: "r2", parents: []string{"r1"}, ops: []fiOp{put("new", "2")}},
	{name: "r3", parents: []string{"r2"}, ops: []fiOp{rm("new"), put("old", "1")}},
	{name: "r4", parents: []string{"r3"}, ops: []fiOp{put("dir1/f", "1")}},
	{name: "r5", parents: []string{"r4"}, ops: []fiOp{rm("dir1/f"), put("dir2/f", "1")}},
	{name: "r6", parents: []string{"r5"}, ops: []fiOp{put("keep", "2")}},
}

// directoryHistory has a directory that is replaced by a file, and a sibling whose
// name starts with the name of the directory.
var directoryHistory = []fiCommit{
	{name: "d0", ops: []fiOp{put("d/a/x", "1"), put("d/b", "1"), put("dd", "1")}},
	{name: "d1", parents: []string{"d0"}, ops: []fiOp{put("d/a/x", "2")}},
	{name: "d2", parents: []string{"d1"}, ops: []fiOp{put("d/a/deep/y", "1")}},
	{name: "d3", parents: []string{"d2"}, ops: []fiOp{rm("d/a/deep/y")}},
	{name: "d4", parents: []string{"d3"}, ops: []fiOp{put("d/b", "2")}},
	{name: "d5", parents: []string{"d4"}, ops: []fiOp{put("dd", "2")}},
	{name: "d6", parents: []string{"d5"}, ops: []fiOp{put("d/a/x", "2")}},
	{name: "d7", parents: []string{"d6"}, ops: []fiOp{rm("d")}},
	{name: "d8", parents: []string{"d7"}, ops: []fiOp{put("d", "1")}},
}

// equivalencePaths are the paths that every history is queried with.
var equivalencePaths = []string{
	"", ".", "/", "a", "a/", "a/x", "a/x/", "b", "keep", "old", "new", "dir", "dir/y",
	"dir/sub", "dir/sub/w", "dir/z", "dir1", "dir1/f", "dir2", "dir2/f", "d", "d/a",
	"d/a/x", "d/a/deep", "d/b", "dd", "missing", "a/../b", "./a", "a//x",
}

// importHistory writes the commits into the repository and returns the object ID of
// each commit by name.
func importHistory(tb testing.TB, cfg config.Cfg, repoPath string, commits []fiCommit) map[string]git.ObjectID {
	tb.Helper()

	marks := make(map[string]int, len(commits))
	var stream strings.Builder
	for i, commit := range commits {
		marks[commit.name] = i + 1
		fmt.Fprintf(&stream, "commit refs/heads/%s\n", commit.name)
		fmt.Fprintf(&stream, "mark :%d\n", i+1)
		fmt.Fprintf(&stream, "committer Tester <tester@example.com> %d +0000\n", 1600000000+i)
		fmt.Fprintf(&stream, "data %d\n%s\n", len(commit.name), commit.name)
		for j, parent := range commit.parents {
			mark, ok := marks[parent]
			require.True(tb, ok, "parent %s of %s is not written yet", parent, commit.name)
			if j == 0 {
				fmt.Fprintf(&stream, "from :%d\n", mark)
			} else {
				fmt.Fprintf(&stream, "merge :%d\n", mark)
			}
		}
		for _, op := range commit.ops {
			if op.remove {
				fmt.Fprintf(&stream, "D %s\n", op.path)
				continue
			}
			fmt.Fprintf(&stream, "M 100644 inline %s\ndata %d\n%s\n", op.path, len(op.content), op.content)
		}
	}

	runGit(tb, cfg, repoPath, nil, stream.String(), "fast-import", "--quiet")

	ids := make(map[string]git.ObjectID, len(commits))
	for _, line := range splitLines(runGit(tb, cfg, repoPath, nil, "", "for-each-ref", "--format=%(objectname) %(refname)")) {
		oid, ref, _ := strings.Cut(line, " ")
		ids[strings.TrimPrefix(ref, "refs/heads/")] = git.ObjectID(oid)
	}
	return ids
}

// newBareRepo creates an empty bare repository in the test storage. Its object format
// follows the test configuration.
func newBareRepo(tb testing.TB, cfg config.Cfg) (*gitalypb.Repository, string) {
	tb.Helper()

	storagePath := cfg.Storages[0].Path
	repo := gittest.InitRepoDir(tb, storagePath, gittest.NewRepositoryName(tb))
	repoPath := filepath.Join(storagePath, repo.GetRelativePath())

	objectFormat := "sha1"
	if gittest.ObjectHashIsSHA256() {
		objectFormat = "sha256"
	}
	gittest.Exec(tb, cfg, "init", "--bare", "--quiet", "--object-format="+objectFormat, repoPath)

	return repo, repoPath
}

// newObjectReader returns an object reader of the repository, released when the test
// ends.
func newObjectReader(tb testing.TB, ctx context.Context, cfg config.Cfg, repo *localrepo.Repo) catfile.ObjectContentReader {
	tb.Helper()

	cache := catfile.NewCache(cfg)
	tb.Cleanup(cache.Stop)

	reader, cancel, err := cache.ObjectReader(ctx, repo)
	require.NoError(tb, err)
	tb.Cleanup(cancel)
	return reader
}

// countingReader counts the tree objects that are read through it.
type countingReader struct {
	catfile.ObjectContentReader
	trees *atomic.Int64
}

func (r countingReader) Object(ctx context.Context, rev git.Revision) (*catfile.Object, error) {
	object, err := r.ObjectContentReader.Object(ctx, rev)
	if err == nil && object.Type == "tree" {
		r.trees.Add(1)
	}
	return object, err
}

func (r countingReader) Queue(ctx context.Context) (catfile.Queue, func(), error) {
	queue, done, err := r.ObjectContentReader.Queue(ctx)
	if err != nil {
		return nil, nil, err
	}
	return countingQueue{Queue: queue, trees: r.trees}, done, nil
}

// countingQueue counts the tree objects that are read through it.
type countingQueue struct {
	catfile.Queue
	trees *atomic.Int64
}

func (q countingQueue) ReadObject(ctx context.Context) (*catfile.Object, error) {
	object, err := q.Queue.ReadObject(ctx)
	if err == nil && object.Type == "tree" {
		q.trees.Add(1)
	}
	return object, err
}

// derivedHistory returns the commits that the Deriver reports for the arguments.
func derivedHistory(ctx context.Context, tb testing.TB, deriver *derived.Deriver, start git.ObjectID, path string, limit int) []string {
	tb.Helper()

	var ids []string
	require.NoError(tb, deriver.PathHistory(ctx, start, path, limit, func(commit *catfile.Commit) error {
		ids = append(ids, commit.GetId())
		return nil
	}))
	return ids
}

// indexHistory returns the commits that the index reports for the arguments.
func indexHistory(ctx context.Context, tb testing.TB, index *Index, start git.ObjectID, path string, limit int) []string {
	tb.Helper()

	var ids []string
	require.NoError(tb, index.PathHistory(ctx, start.String(), path, limit, func(commit Commit) error {
		ids = append(ids, commit.ID)
		return nil
	}))
	return ids
}

// TestIndex_MatchesDeriverPathHistory requires the index to report exactly what the
// existing (*derived.Deriver).PathHistory reports, for every commit, every path and
// limits that stop the walk early.
func TestIndex_MatchesDeriverPathHistory(t *testing.T) {
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
			cachedDeriver, err := derived.NewDeriver(ctx, logger, repo, reader, true)
			require.NoError(t, err)
			uncachedDeriver, err := derived.NewDeriver(ctx, logger, repo, reader, false)
			require.NoError(t, err)

			var trees atomic.Int64
			counting := countingReader{ObjectContentReader: reader, trees: &trees}
			stored := NewIndexFor(counting, derived.NewStore(repoPath))
			uncached := NewIndex(NewObjectReader(counting), nil)

			for _, commit := range commits {
				for _, path := range equivalencePaths {
					require.True(t, Supports(path), path)

					for _, limit := range []int{1000, 2} {
						want := derivedHistory(ctx, t, cachedDeriver, ids[commit.name], path, limit)
						require.Equal(t, want, derivedHistory(ctx, t, uncachedDeriver, ids[commit.name], path, limit),
							"uncached deriver: %q from %s, limit %d", path, commit.name, limit)
						require.Equal(t, want, indexHistory(ctx, t, stored, ids[commit.name], path, limit),
							"stored index: %q from %s, limit %d", path, commit.name, limit)
						require.Equal(t, want, indexHistory(ctx, t, uncached, ids[commit.name], path, limit),
							"uncached index: %q from %s, limit %d", path, commit.name, limit)
					}
				}
			}
		})
	}
}

// TestIndex_ZeroLimitReportsNothing matches the Deriver, which reports nothing for a
// limit that is not positive.
func TestIndex_ZeroLimitReportsNothing(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := newBareRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	ids := importHistory(t, cfg, repoPath, linearHistory)

	reader := newObjectReader(t, ctx, cfg, repo)
	deriver, err := derived.NewDeriver(ctx, testhelper.NewLogger(t), repo, reader, true)
	require.NoError(t, err)
	index := NewIndex(NewObjectReader(reader), nil)

	for _, limit := range []int{0, -1} {
		require.Empty(t, derivedHistory(ctx, t, deriver, ids["l9"], "a/x", limit))
		require.Empty(t, indexHistory(ctx, t, index, ids["l9"], "a/x", limit))
	}
}

// TestWarm_PersistsSummaries requires the warmer to store the summaries of its
// commits, so that the index answers from the store without reading trees.
func TestWarm_PersistsSummaries(t *testing.T) {
	t.Parallel()

	require.Contains(t, derived.Warmers(), kindName)

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := newBareRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	ids := importHistory(t, cfg, repoPath, linearHistory)

	reader := newObjectReader(t, ctx, cfg, repo)
	store := derived.NewStore(repoPath)

	var commits []git.ObjectID
	for _, commit := range linearHistory {
		commits = append(commits, ids[commit.name])
	}
	require.NoError(t, Warm(ctx, derived.WarmEnv{Logger: testhelper.NewLogger(t), Repo: repo, Reader: reader, Store: store}, commits))

	var trees atomic.Int64
	index := NewIndexFor(countingReader{ObjectContentReader: reader, trees: &trees}, store)
	// a/x is added in l0, changed in l2, removed in l3 and added again in l4.
	require.Equal(t, []string{ids["l4"].String(), ids["l3"].String(), ids["l2"].String(), ids["l0"].String()},
		indexHistory(ctx, t, index, ids["l9"], "a/x", unlimited),
	)
	require.Equal(t, int64(0), index.Computed())
	require.Zero(t, trees.Load())
}

// BenchmarkPathHistory compares the trees that the Deriver, the index and the
// service's filtered Deriver (cold, with warmed summaries and repeated) read for the
// first-parent history of one path in a 2,000-commit linear history.
func BenchmarkPathHistory(b *testing.B) {
	ctx := testhelper.Context(b)
	cfg := testcfg.Build(b)
	repoProto, repoPath := newBareRepo(b, cfg)
	repo := localrepo.NewTestRepo(b, cfg, repoProto)

	const length = 2000
	commits := make([]fiCommit, 0, length)
	for i := 0; i < length; i++ {
		commit := fiCommit{name: fmt.Sprintf("c%d", i)}
		if i > 0 {
			commit.parents = []string{fmt.Sprintf("c%d", i-1)}
		}
		if i%100 == 0 {
			commit.ops = []fiOp{put("a/x", fmt.Sprint(i))}
		} else {
			commit.ops = []fiOp{put("b/y", fmt.Sprint(i))}
		}
		commits = append(commits, commit)
	}
	ids := importHistory(b, cfg, repoPath, commits)
	start := ids[fmt.Sprintf("c%d", length-1)]

	reader := newObjectReader(b, ctx, cfg, repo)
	logger := testhelper.NewLogger(b)

	run := func(b *testing.B, history func(counting countingReader) []string) {
		var trees atomic.Int64
		counter := &trees
		for i := 0; i < b.N; i++ {
			got := history(countingReader{ObjectContentReader: reader, trees: counter})
			require.Len(b, got, length/100)
		}
		b.ReportMetric(float64(trees.Load())/float64(b.N), "tree-reads/op")
	}

	b.Run("deriver", func(b *testing.B) {
		run(b, func(counting countingReader) []string {
			deriver, err := derived.NewDeriver(ctx, logger, repo, counting, false)
			require.NoError(b, err)
			return derivedHistory(ctx, b, deriver, start, "a/x", length)
		})
	})

	b.Run("index", func(b *testing.B) {
		run(b, func(counting countingReader) []string {
			return indexHistory(ctx, b, NewIndex(NewObjectReader(counting), nil), start, "a/x", length)
		})
	})

	all := make([]git.ObjectID, 0, length)
	for _, commit := range commits {
		all = append(all, ids[commit.name])
	}
	warmed := derived.NewStore(testhelper.TempDir(b))
	require.NoError(b, Warm(ctx, derived.WarmEnv{Logger: logger, Repo: repo, Reader: reader, Store: warmed}, all))

	b.Run("index-persisted", func(b *testing.B) {
		run(b, func(counting countingReader) []string {
			return indexHistory(ctx, b, NewIndexFor(counting, warmed), start, "a/x", length)
		})
	})

	// The service variants run the Deriver with its store, as DerivedDataService does,
	// with the stored changed-paths summaries as the pre-filter. A cold query finds
	// neither stored chain values nor summaries (eager warming is off by default), a
	// warmed one finds the summaries only, and a repeated one finds the chain values
	// of the queries before it.
	service := func(b *testing.B, summaries *derived.Store, cold bool) {
		run(b, func(counting countingReader) []string {
			if cold {
				b.StopTimer()
				require.NoError(b, os.RemoveAll(filepath.Join(repoPath, "graft")))
				b.StartTimer()
			}
			deriver, err := derived.NewDeriver(ctx, logger, repo, counting, true)
			require.NoError(b, err)
			return filteredHistory(ctx, b, deriver, NewIndexFor(counting, summaries), start, "a/x", length)
		})
	}

	b.Run("service-cold", func(b *testing.B) {
		service(b, derived.NewStore(repoPath), true)
	})

	b.Run("service-warmed", func(b *testing.B) {
		service(b, warmed, true)
	})

	b.Run("service-repeated", func(b *testing.B) {
		service(b, derived.NewStore(repoPath), false)
	})
}
