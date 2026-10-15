package changedpaths

// Black-box spec tests for WP27 (changed-paths kind), written from PLAN3.md
// only. Spec-named identifiers used here:
//   derived.Warmers() []string, derived.Store.Kind(name, version) KindStore,
//   KindStore.Get(commit) ([]byte, bool, error), derived.NewPool(...),
//   (*Pool).Enqueue(repo, commits), (*Pool).Close(), derived.WarmEnv.
// The spec does not name constructors for Store, Pool's open-func environment
// or the test repository wiring for derived.WarmEnv. The helpers
// specStore / specPool / specRepo below are the ONLY places that depend on
// those constructors and must be adapted to the real ones at merge time.
// Everything else asserts exactly what the spec states.
//
// Adapted at release time: the package adopted the draft's summary encoding
// (see the RECONCILIATION section of PLAN3.md), which stores the sorted paths
// length-prefixed instead of newline-joined and an overflow as a flag instead
// of "*". specValue decodes the stored value and renders it in the spec's
// textual form, so the tests still assert the spec's content. The pool drops
// batches still queued at Close, so specWarm waits for the values before it
// closes the pool. Each test uses one configuration (specCfg) for its
// repository and commits, and the large trees reuse one blob.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/graft/derived"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
)

const specKind = "changed-paths"

// specConfigs holds the one configuration of each test: the repository and
// the commits written into it must use the same storage.
var specConfigs sync.Map

// specCfg returns the configuration of the test, building it on first use.
// The tests call it where the spec draft called testcfg.Build.
func specCfg(t *testing.T) config.Cfg {
	t.Helper()
	if value, ok := specConfigs.Load(t); ok {
		return value.(config.Cfg)
	}
	cfg := testcfg.Build(t)
	specConfigs.Store(t, cfg)
	t.Cleanup(func() { specConfigs.Delete(t) })
	return cfg
}

// specStore, specRepo and specWarm are the adapter points (see header).
func specStore(t *testing.T) *derived.Store {
	t.Helper()
	return derived.NewStore(testhelper.TempDir(t))
}

func specRepo(t *testing.T) (storage.Repository, string) {
	t.Helper()
	ctx := testhelper.Context(t)
	repo, repoPath := gittest.CreateRepository(t, ctx, specCfg(t), gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	return repo, repoPath
}

// specWarm computes the changed-paths kind for commits through the warmer
// pool and waits until every value is stored.
func specWarm(t *testing.T, store *derived.Store, repo storage.Repository, commits []git.ObjectID) {
	t.Helper()
	ctx := testhelper.Context(t)
	cfg := specCfg(t)

	localRepo := localrepo.NewTestRepo(t, cfg, repo)
	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)
	open := func(ctx context.Context, _ storage.Repository) (derived.WarmEnv, func(), error) {
		reader, cancel, err := catfileCache.ObjectReader(ctx, localRepo)
		if err != nil {
			return derived.WarmEnv{}, nil, err
		}
		return derived.WarmEnv{Logger: testhelper.NewLogger(t), Repo: localRepo, Reader: reader, Store: store}, cancel, nil
	}

	pool := derived.NewPool(testhelper.NewLogger(t), 2, 16, open)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(runCtx)
	}()
	defer func() {
		pool.Close()
		cancel()
		<-done
	}()

	pool.Enqueue(repo, commits)

	kind := store.Kind(specKind, 1)
	deadline := time.Now().Add(60 * time.Second)
	for _, commit := range commits {
		for {
			_, ok, err := kind.Get(commit)
			if err != nil {
				t.Fatalf("reading stored changed-paths value for %s: %v", commit, err)
			}
			if ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("changed-paths value of %s not stored within 60s", commit)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func specValue(t *testing.T, store *derived.Store, commit git.ObjectID) string {
	t.Helper()
	v, ok, err := store.Kind(specKind, 1).Get(commit)
	if err != nil {
		t.Fatalf("reading stored changed-paths value for %s: %v", commit, err)
	}
	if !ok {
		t.Fatalf("expected a stored changed-paths value for %s after warming, found none", commit)
	}
	summary, err := decodeSummary(v)
	if err != nil {
		t.Fatalf("decoding stored changed-paths value for %s: %v", commit, err)
	}
	if summary.Overflow {
		return "*"
	}
	return strings.Join(summary.paths, "\n")
}

func TestSpecChangedPathsKindRegistered(t *testing.T) {
	names := derived.Warmers()
	if !sort.StringsAreSorted(names) {
		t.Errorf("derived.Warmers() must be sorted, got %v", names)
	}
	found := false
	for _, n := range names {
		if n == specKind {
			found = true
		}
	}
	if !found {
		t.Errorf("expected warmer %q to be registered by the changedpaths package init, got %v", specKind, names)
	}
}

func TestSpecChangedPathsRootListsAllPathsSortedNewlineJoined(t *testing.T) {
	repo, repoPath := specRepo(t)
	cfg := specCfg(t)
	_ = cfg
	// gittest tree entries cannot hold slashes: dir/c.txt is written as a subtree.
	dir := gittest.WriteTree(t, specCfg(t), repoPath, []gittest.TreeEntry{
		{Mode: "100644", Path: "c.txt", Content: "c"},
	})
	commit := gittest.WriteCommit(t, specCfg(t), repoPath,
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "b.txt", Content: "b"},
			gittest.TreeEntry{Mode: "100644", Path: "a.txt", Content: "a"},
			gittest.TreeEntry{Mode: "040000", Path: "dir", OID: dir},
		))
	store := specStore(t)
	specWarm(t, store, repo, []git.ObjectID{commit})

	want := "a.txt\nb.txt\ndir/c.txt"
	if got := specValue(t, store, commit); got != want {
		t.Errorf("root commit value = %q, want all paths sorted and newline-joined %q", got, want)
	}
}

func TestSpecChangedPathsComparedToFirstParentOnly(t *testing.T) {
	repo, repoPath := specRepo(t)
	cfg := specCfg(t)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "keep.txt", Content: "k"},
		gittest.TreeEntry{Mode: "100644", Path: "gone.txt", Content: "g"},
	))
	left := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "keep.txt", Content: "k"},
		gittest.TreeEntry{Mode: "100644", Path: "left.txt", Content: "l"},
	))
	right := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "keep.txt", Content: "changed-on-right"},
	))
	merge := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(left, right), gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "keep.txt", Content: "changed-on-right"},
		gittest.TreeEntry{Mode: "100644", Path: "left.txt", Content: "l"},
	))
	store := specStore(t)
	specWarm(t, store, repo, []git.ObjectID{base, left, right, merge})

	// Merge vs FIRST parent (left): right's change to keep.txt must be
	// reported, and gone.txt is absent in both so it is not listed.
	if got, want := specValue(t, store, merge), "keep.txt"; got != want {
		t.Errorf("merge value = %q, want paths changed vs first parent %q", got, want)
	}
}

func TestSpecChangedPathsRenameIsDeletePlusAdd(t *testing.T) {
	repo, repoPath := specRepo(t)
	cfg := specCfg(t)
	parent := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "old.txt", Content: "same content so git would detect a rename"},
	))
	child := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(parent), gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "new.txt", Content: "same content so git would detect a rename"},
	))
	store := specStore(t)
	specWarm(t, store, repo, []git.ObjectID{parent, child})

	if got, want := specValue(t, store, child), "new.txt\nold.txt"; got != want {
		t.Errorf("rename value = %q, want %q (renames are not detected: delete and add)", got, want)
	}
}

func TestSpecChangedPathsAboveLimitIsStar(t *testing.T) {
	repo, repoPath := specRepo(t)
	cfg := specCfg(t)
	blob := gittest.WriteBlob(t, cfg, repoPath, []byte("x"))
	entries := make([]gittest.TreeEntry, 0, 20001)
	for i := 0; i < 20001; i++ {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: fmt.Sprintf("f%05d", i), OID: blob})
	}
	commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(entries...))
	store := specStore(t)
	specWarm(t, store, repo, []git.ObjectID{commit})

	if got := specValue(t, store, commit); got != "*" {
		t.Errorf("more than 20,000 changed paths must store the single line %q, got %d bytes", "*", len(got))
	}
}

func TestSpecChangedPathsAtLimitIsExplicitList(t *testing.T) {
	repo, repoPath := specRepo(t)
	cfg := specCfg(t)
	blob := gittest.WriteBlob(t, cfg, repoPath, []byte("x"))
	entries := make([]gittest.TreeEntry, 0, 20000)
	for i := 0; i < 20000; i++ {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: fmt.Sprintf("f%05d", i), OID: blob})
	}
	commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(entries...))
	store := specStore(t)
	specWarm(t, store, repo, []git.ObjectID{commit})

	got := specValue(t, store, commit)
	if got == "*" {
		t.Fatalf("exactly 20,000 changed paths must not collapse to %q", "*")
	}
	if n := len(strings.Split(got, "\n")); n != 20000 {
		t.Errorf("expected 20000 listed paths, got %d", n)
	}
}

func TestSpecChangedPathsVersionedKey(t *testing.T) {
	repo, repoPath := specRepo(t)
	commit := gittest.WriteCommit(t, specCfg(t), repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "a.txt", Content: "a"},
	))
	store := specStore(t)
	specWarm(t, store, repo, []git.ObjectID{commit})

	if _, ok, err := store.Kind(specKind, 1).Get(commit); err != nil || !ok {
		t.Fatalf("kind %q version 1 must be stored: ok=%v err=%v", specKind, ok, err)
	}
	if _, ok, err := store.Kind(specKind, 2).Get(commit); err != nil || ok {
		t.Errorf("version 2 must not see version 1 values (version bump invalidates): ok=%v err=%v", ok, err)
	}
}

func TestSpecChangedPathsPoolDrainsOnClose(t *testing.T) {
	repo, repoPath := specRepo(t)
	cfg := specCfg(t)
	ctx := testhelper.Context(t)
	var commits []git.ObjectID
	parent := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Mode: "100644", Path: "base.txt", Content: "b"},
	))
	commits = append(commits, parent)
	for i := 0; i < 5; i++ {
		c := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(parent), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: fmt.Sprintf("n%d.txt", i), Content: "n"},
		))
		commits = append(commits, c)
	}
	_ = ctx
	store := specStore(t)
	specWarm(t, store, repo, commits) // Enqueue + Close must drain before returning

	for _, c := range commits {
		if _, ok, err := store.Kind(specKind, 1).Get(c); err != nil || !ok {
			t.Errorf("commit %s enqueued before Close must be computed when Close returns: ok=%v err=%v", c, ok, err)
		}
	}
}
