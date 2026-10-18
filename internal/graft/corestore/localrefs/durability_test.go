package localrefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// durabilityFixture is a reftable repository with a few references, written
// one transaction each so that its stack has several tables.
type durabilityFixture struct {
	ctx     context.Context
	cfg     config.Cfg
	locator storage.Locator
	repo    RepoRef
	path    string
	commit  git.ObjectID
	synced  *syncRecorder
}

// syncRecorder records the paths made durable, and fails them while failWith
// is set.
type syncRecorder struct {
	mu       sync.Mutex
	paths    []string
	failWith error
}

func (s *syncRecorder) sync(f *os.File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return s.failWith
	}
	s.paths = append(s.paths, f.Name())
	return f.Sync()
}

func (s *syncRecorder) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func (s *syncRecorder) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWith = err
}

func setupDurability(t *testing.T) durabilityFixture {
	t.Helper()
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	pb, path := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
		ReferenceBackend:       git.ReferenceBackendReftables,
	})
	commit := gittest.WriteCommit(t, cfg, path)
	for _, name := range []string{"refs/heads/main", "refs/heads/a", "refs/heads/b"} {
		gittest.WriteRef(t, cfg, path, git.ReferenceName(name), commit)
	}
	return durabilityFixture{
		ctx:     ctx,
		cfg:     cfg,
		locator: config.NewLocator(cfg),
		repo:    RepoRef{Storage: pb.GetStorageName(), RelativePath: pb.GetRelativePath()},
		path:    path,
		commit:  commit,
		synced:  &syncRecorder{},
	}
}

func (f durabilityFixture) durability(t *testing.T, bootID string, idle time.Duration) *Durability {
	t.Helper()
	d, err := NewDurability(f.locator, idle, WithBootID(bootID), WithSync(f.synced.sync))
	require.NoError(t, err)
	return d
}

func (f durabilityFixture) markerPath(t *testing.T, d *Durability) string {
	t.Helper()
	p, err := d.markerPath(f.ctx, f.repo, f.path)
	require.NoError(t, err)
	return p
}

func (f durabilityFixture) tables(t *testing.T) []string {
	t.Helper()
	names, err := readTablesList(filepath.Join(f.path, "reftable", "tables.list"))
	require.NoError(t, err)
	require.NotEmpty(t, names)
	return names
}

func TestMarkDirtyWritesOneDurableMarker(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)

	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))

	content, err := os.ReadFile(f.markerPath(t, d))
	require.NoError(t, err)
	require.JSONEq(t, `{"storage":"`+f.repo.Storage+`","relative_path":"`+f.repo.RelativePath+`","boot_id":"boot-a"}`, string(content))
	require.Equal(t, 1.0, testutil.ToFloat64(d.marked))
	require.Equal(t, []string{f.path}, d.Dirty())

	// The marker file and its directory were made durable before MarkDirty
	// returned.
	recorded := f.synced.recorded()
	require.Len(t, recorded, 2)
	require.Equal(t, filepath.Dir(f.markerPath(t, d)), recorded[1])
	require.Equal(t, filepath.Join(f.cfg.Storages[0].Path, MarkerDirName), filepath.Dir(f.markerPath(t, d)))
}

func TestSettleMakesTheStackDurableAndRemovesTheMarker(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)

	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, d.Settle(f.path))

	require.NoFileExists(t, f.markerPath(t, d))
	require.Empty(t, d.Dirty())
	require.Equal(t, 1.0, testutil.ToFloat64(d.cleared))
	require.Equal(t, 0.0, testutil.ToFloat64(d.dirty))

	recorded := f.synced.recorded()
	for _, table := range f.tables(t) {
		require.Contains(t, recorded, filepath.Join(f.path, "reftable", table))
	}
	require.Contains(t, recorded, filepath.Join(f.path, "reftable", "tables.list"))
	require.Contains(t, recorded, filepath.Join(f.path, "reftable"))
	// The lock taken for the removal is released.
	require.NoFileExists(t, filepath.Join(f.path, "reftable", "tables.list.lock"))
}

func TestSettleKeepsTheMarkerWhileWritesMayGoOn(t *testing.T) {
	t.Parallel()

	t.Run("not idle", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.durability(t, "boot-a", time.Hour)

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		require.NoError(t, d.Settle(f.path))
		require.FileExists(t, f.markerPath(t, d))
		// The stack was made durable all the same.
		require.Contains(t, f.synced.recorded(), filepath.Join(f.path, "reftable", f.tables(t)[0]))
	})

	t.Run("a transaction between Prepared and Committed", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.durability(t, "boot-a", 0)

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		d.Begin(f.path, key(1))
		require.NoError(t, d.Settle(f.path))
		require.FileExists(t, f.markerPath(t, d))

		// An end of another key does not count; the end of the transaction does.
		d.End(f.path, key(2))
		require.NoError(t, d.Settle(f.path))
		require.FileExists(t, f.markerPath(t, d))

		d.End(f.path, key(1))
		require.NoError(t, d.Settle(f.path))
		require.NoFileExists(t, f.markerPath(t, d))
		require.Equal(t, 0.0, testutil.ToFloat64(d.inflight))
	})

	t.Run("git holds the reference lock", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.durability(t, "boot-a", 0)

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		lock := filepath.Join(f.path, "reftable", "tables.list.lock")
		require.NoError(t, os.WriteFile(lock, nil, 0o666))

		require.NoError(t, d.Settle(f.path))
		require.FileExists(t, f.markerPath(t, d))
		require.FileExists(t, lock, "the durable step removed a lock it did not take")
	})
}

func TestSettleReportsAFailedWriteAsUntrusted(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)

	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	writeback := errors.New("input/output error")
	f.synced.fail(writeback)

	err := d.Settle(f.path)
	require.ErrorIs(t, err, ErrUntrusted)
	require.ErrorIs(t, err, writeback)
	var untrusted *UntrustedError
	require.ErrorAs(t, err, &untrusted)
	require.Equal(t, f.repo, untrusted.Repo)
	require.Equal(t, f.path, untrusted.Path)

	// The repository stays dirty until the caller moves it aside.
	require.FileExists(t, f.markerPath(t, d))
	require.Equal(t, []string{f.path}, d.Dirty())

	require.NoError(t, d.Forget(f.ctx, f.repo, f.path))
	require.NoFileExists(t, f.markerPath(t, d))
	require.Empty(t, d.Dirty())
	require.Equal(t, 0.0, testutil.ToFloat64(d.dirty))
}

func TestSettleDropsTheMarkerOfARepositoryThatIsGone(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)

	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, os.Rename(f.path, f.path+".moved"))
	t.Cleanup(func() { require.NoError(t, os.Rename(f.path+".moved", f.path)) })

	require.NoError(t, d.Settle(f.path))
	require.NoFileExists(t, f.markerPath(t, d))
	require.Empty(t, d.Dirty())
}

func TestRecoverTellsAKernelRestartFromAProcessRestart(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	before := f.durability(t, "boot-a", time.Hour)
	require.NoError(t, before.MarkDirty(f.ctx, f.repo, f.path))
	storages := []string{f.repo.Storage}

	// The same boot: the process restarted, the kernel kept every write.
	sameBoot := f.durability(t, "boot-a", 0)
	found, err := sameBoot.Recover(f.ctx, storages)
	require.NoError(t, err)
	require.Equal(t, []Recovered{{Repo: f.repo, Path: f.path, Reboot: false}}, found)
	require.Equal(t, []string{f.path}, sameBoot.Dirty())
	require.NoError(t, sameBoot.Settle(f.path))
	require.NoFileExists(t, f.markerPath(t, sameBoot))

	// Another boot: the writes may not have reached the disk.
	require.NoError(t, f.durability(t, "boot-a", time.Hour).MarkDirty(f.ctx, f.repo, f.path))
	otherBoot := f.durability(t, "boot-b", 0)
	found, err = otherBoot.Recover(f.ctx, storages)
	require.NoError(t, err)
	require.Equal(t, []Recovered{{Repo: f.repo, Path: f.path, Reboot: true}}, found)
	require.Empty(t, otherBoot.Dirty())
}

// TestRecoverRemovesTheLockOfADurableStepThatDied verifies that a reference
// lock left by a durable step of a process that died is removed at startup,
// and that a lock of git is not.
func TestRecoverRemovesTheLockOfADurableStepThatDied(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	lock := filepath.Join(f.path, "reftable", "tables.list.lock")

	require.NoError(t, f.durability(t, "boot-a", 0).MarkDirty(f.ctx, f.repo, f.path))
	unlock, ok, err := lockStack(f.path)
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = lockStack(f.path)
	require.NoError(t, err)
	require.False(t, ok, "the lock is exclusive")
	entries, err := os.ReadDir(f.path)
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), ".graft-durable-step-"), "temporary file %s left", entry.Name())
	}

	// The process dies holding the lock; the next one of the same boot removes it.
	_, err = f.durability(t, "boot-a", 0).Recover(f.ctx, []string{f.repo.Storage})
	require.NoError(t, err)
	require.NoFileExists(t, lock)
	require.Error(t, unlock(), "the lock was already removed")

	// A lock of git is kept.
	require.NoError(t, os.WriteFile(lock, []byte("0x000000000001-0x000000000003-abcdef01.ref\n"), 0o666))
	_, err = f.durability(t, "boot-a", 0).Recover(f.ctx, []string{f.repo.Storage})
	require.NoError(t, err)
	require.FileExists(t, lock)
}

func TestRecoverRefusesACorruptMarker(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)
	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))

	marker := f.markerPath(t, d)
	incomplete := filepath.Join(filepath.Dir(marker), ".marker-123.tmp")
	require.NoError(t, os.WriteFile(incomplete, []byte("{"), 0o600))

	found, err := f.durability(t, "boot-b", 0).Recover(f.ctx, []string{f.repo.Storage})
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.NoFileExists(t, incomplete, "an incomplete marker write is removed")

	require.NoError(t, os.WriteFile(marker, []byte(`{"storage":"`), 0o600))
	_, err = f.durability(t, "boot-b", 0).Recover(f.ctx, []string{f.repo.Storage})
	require.ErrorContains(t, err, "corrupt dirty marker")
}

func TestNilDurabilityIsOff(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	var d *Durability
	require.False(t, d.Enabled())
	require.NoError(t, d.MarkDirty(ctx, RepoRef{}, "path"))
	require.NoError(t, d.Settle("path"))
	d.Begin("path", key(1))
	d.End("path", key(1))
	found, err := d.Recover(ctx, []string{"default"})
	require.NoError(t, err)
	require.Empty(t, found)
}

// TestTornReftableStackReadsAsAnEmptyRepository is the experiment behind the
// LOCAL REFERENCE WRITES RULE (doc/graft/PLAN7.md): a reftable stack that a
// crash tore (a table whose data never reached the disk) is read by git as a
// repository without references, without an error. Only writes fail. So a torn
// stack must never be served: nothing in git would tell.
func TestTornReftableStackReadsAsAnEmptyRepository(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)

	refs := gittest.Exec(t, f.cfg, "-C", f.path, "for-each-ref", "--format=%(refname)")
	require.Equal(t, "refs/heads/a\nrefs/heads/b\nrefs/heads/main", strings.TrimSpace(string(refs)))

	tables := f.tables(t)
	newest := filepath.Join(f.path, "reftable", tables[len(tables)-1])
	require.NoError(t, os.Truncate(newest, 0))

	refs = gittest.Exec(t, f.cfg, "-C", f.path, "for-each-ref", "--format=%(refname)")
	require.Empty(t, strings.TrimSpace(string(refs)), "git reports the references of a torn stack")

	cmd := gittest.NewCommand(t, f.cfg, "-C", f.path, "update-ref", "refs/heads/new", f.commit.String())
	output, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "corrupt reftable file")
}
