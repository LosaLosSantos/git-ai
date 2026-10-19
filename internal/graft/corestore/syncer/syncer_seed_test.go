package syncer_test

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// seedFixture is a repository written on node a with a checkpoint followed by
// changesets, and a fresh node b whose first seed the test holds at the "seed"
// point (after the checkpoint's references are read, before its pack is
// indexed) until release is closed. b's rounds are long, so a round waits for
// the seed and returns its result.
type seedFixture struct {
	a, b    *env
	src     source
	repo    corestore.Repository
	cp      corestore.Checkpoint
	seeding chan struct{}
	release chan struct{}
}

func newSeedFixture(t *testing.T) *seedFixture {
	t.Helper()

	f := &seedFixture{a: newEnv(t, envOptions{}), seeding: make(chan struct{}), release: make(chan struct{})}
	f.src = f.a.newSource(t)
	c := f.a.writeCommit(t, f.src)
	f.repo = f.a.register(t, f.a.key(localRelPath), f.src, c)
	c = f.a.advanceMain(t, f.repo.ID, f.src, c)
	f.cp = f.a.checkpoint(t, f.repo.ID, f.src)
	f.a.advanceMain(t, f.repo.ID, f.src, c)

	f.b = f.a.secondNode(t, sqlstore.SQLite)
	f.b.core.SyncTimeout = time.Minute
	var once sync.Once
	f.b.sy.SetTestHook(func(point string) {
		if point != "seed" {
			return
		}
		first := false
		once.Do(func() { first = true; close(f.seeding) })
		if first {
			<-f.release
		}
	})
	return f
}

// ensureFresh runs EnsureFresh on b in the background.
func (f *seedFixture) ensureFresh() <-chan error {
	result := make(chan error, 1)
	go func() { result <- f.b.sy.EnsureFresh(f.b.ctx, f.b.pbRepo(localRelPath)) }()
	return result
}

// TestSeedSupersededByANewerCheckpoint: a checkpoint that becomes C_sync while
// a seed from an older one runs makes that seed install nothing; the round
// seeds from the newest and catches up (final minor 3).
func TestSeedSupersededByANewerCheckpoint(t *testing.T) {
	t.Parallel()
	f := newSeedFixture(t)

	result := f.ensureFresh()
	<-f.seeding
	newer := f.a.checkpoint(t, f.repo.ID, f.src)
	require.Greater(t, newer.Seq, f.cp.Seq)
	f.a.ageCheckpoint(t, f.repo.ID, newer.Seq, f.b.core.GCGrace+time.Minute)
	close(f.release)

	require.NoError(t, <-result)
	require.Equal(t, float64(2), counter(t, f.b.sy, "graft_storage_sync_checkpoint_seeds_total", ""), "the superseded seed, then one from the newest")
	repo, err := f.b.core.Meta.GetRepository(f.b.ctx, f.repo.Key)
	require.NoError(t, err)
	f.b.verifySynced(t, repo)
}

// TestSeedPastItsDeadline: a seed held past its deadline (readAt + W) while
// its checkpoint pack is collected, as garbage collection may then do, fails
// as a seed past its deadline, Unavailable, not as a missing pack; the next
// request starts again (final minor 2).
func TestSeedPastItsDeadline(t *testing.T) {
	t.Parallel()
	f := newSeedFixture(t)
	// W = gc_grace - gc_margin = 1 s on node b.
	f.b.core.GCGrace = 2 * time.Second
	f.b.core.GCMargin = time.Second

	result := f.ensureFresh()
	<-f.seeding
	require.NoError(t, f.a.core.Blobs.Delete(f.a.ctx, f.cp.PackKey), "collected after the deadline")
	// The deadline is a time: the seed is held until it passed.
	<-time.After(f.b.core.WriteDeadline() + 200*time.Millisecond)
	close(f.release)

	err := <-result
	require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "seed deadline passed")
	_, ok, appliedErr := f.b.sy.Applied(f.b.ctx, f.b.pbRepo(localRelPath))
	require.NoError(t, appliedErr)
	require.False(t, ok, "nothing is installed")
}

// TestSeedOfARepositoryRemovedAndCreatedAgain: the repository is removed and
// registered again under a new ID while a seed of the old one runs; the seed
// installs nothing and fails as such, and the next request hydrates the new
// repository (final minor 3).
func TestSeedOfARepositoryRemovedAndCreatedAgain(t *testing.T) {
	t.Parallel()
	f := newSeedFixture(t)

	result := f.ensureFresh()
	<-f.seeding
	removed, err := f.a.core.Meta.DeleteRepository(f.a.ctx, f.repo.Key)
	require.NoError(t, err)
	require.True(t, removed)
	again := f.a.register(t, f.repo.Key, f.src, f.a.writeCommit(t, f.src))
	require.NotEqual(t, f.repo.ID, again.ID)
	close(f.release)

	err = <-result
	require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "the repository was removed during the seed")

	require.NoError(t, f.b.sy.EnsureFresh(f.b.ctx, f.b.pbRepo(localRelPath)))
	again, err = f.b.core.Meta.GetRepository(f.b.ctx, again.Key)
	require.NoError(t, err)
	f.b.verifySynced(t, again)
}

// TestStopSeedsEndsTheSeedJobs: StopSeeds cancels a running seed, waits for it
// and its build directory to be gone, and no seed starts after it (final
// minor 3).
func TestStopSeedsEndsTheSeedJobs(t *testing.T) {
	t.Parallel()
	f := newSeedFixture(t)

	result := f.ensureFresh()
	<-f.seeding
	stopped := make(chan struct{})
	go func() {
		f.b.sy.StopSeeds()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("StopSeeds returned while the seed runs")
	default:
	}
	close(f.release)
	<-stopped

	err := <-result
	require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	tmpDir, tmpErr := f.b.locator.TempDir(f.b.storageName)
	require.NoError(t, tmpErr)
	builds, globErr := filepath.Glob(filepath.Join(tmpDir, "hydrate-*"))
	require.NoError(t, globErr)
	require.Empty(t, builds, "the build directory is removed")

	err = f.b.sy.EnsureFresh(f.b.ctx, f.b.pbRepo(localRelPath))
	require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "the server is stopping")
}

// TestCleanBuildsRemovesOnlyDeadBuilds: at startup the build directories of
// a process that died are removed (their lock is free, or they never got one
// and are old), those of a running process are kept (final minor 3).
func TestCleanBuildsRemovesOnlyDeadBuilds(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	tmpDir, err := e.locator.TempDir(e.storageName)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(tmpDir, 0o700))

	mkdir := func(name string) string {
		dir := filepath.Join(tmpDir, name)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "repo.git"), 0o700))
		return dir
	}
	lockFile := func(dir string) *os.File {
		f, err := os.OpenFile(filepath.Join(dir, "build.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })
		return f
	}

	running := mkdir("hydrate-running")
	require.NoError(t, syscall.Flock(int(lockFile(running).Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	dead := mkdir("hydrate-dead")
	require.NoError(t, lockFile(dead).Close(), "a lock file nobody holds: its process died")
	oldUnlocked := mkdir("hydrate-old-unlocked")
	require.NoError(t, os.Chtimes(oldUnlocked, time.Now().Add(-2*time.Minute), time.Now().Add(-2*time.Minute)))
	newUnlocked := mkdir("hydrate-new-unlocked")
	other := mkdir("stale-other")

	require.NoError(t, e.sy.CleanBuilds(e.ctx, []string{e.storageName}))
	for dir, kept := range map[string]bool{running: true, dead: false, oldUnlocked: false, newUnlocked: true, other: true} {
		_, err := os.Stat(dir)
		if kept {
			require.NoError(t, err, "%s is kept", dir)
		} else {
			require.ErrorIs(t, err, os.ErrNotExist, "%s is removed", dir)
		}
	}
}
