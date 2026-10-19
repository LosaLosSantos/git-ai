package localrefs

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
)

// TestMarkerLifecycleModel drives random interleavings of everything that
// touches a dirty marker against a reference model: transactions that mark the
// repository (MarkDirty), git processes that start and exit (Begin and end) and
// write tables without fsync, durable steps (Settle, final or not), failed
// fsyncs, the repository moved aside (Forget) and hydrated again, restarts of
// the process (Recover of the same boot, with git processes of the previous
// owner still finishing a write) and kernel restarts (Recover of another
// boot), and the acceptance in place of a repository after a kernel restart.
// After every step it asserts the two invariants of the LOCAL REFERENCE WRITES
// RULE (doc/graft/PLAN7.md):
//
//  1. while a git process may still write the stack without fsync (it marked
//     the repository and has not exited, or it is a process of the previous
//     owner in the middle of a write), or the stack at the path holds a table,
//     a table list or a directory entry written without fsync since, the
//     marker is on disk;
//  2. an untrusted repository is reported untrusted (the syncer never serves
//     it) and keeps its marker, until it is moved aside or accepted.
//
// The model learns what is durable from the fsyncs the durability makes (its
// sync function), not from what the code under test reports. The steps are
// sequential: what git's reference lock excludes (a write while a durable step
// holds it) is not modelled; dropMarker refuses to remove the marker of an
// existing repository without that lock.
func TestMarkerLifecycleModel(t *testing.T) {
	t.Parallel()

	for seed := int64(1); seed <= 40; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			runMarkerModel(t, seed, 250)
		})
	}
}

// TestDropMarkerGuards verifies the guards of dropMarker that no caller
// reaches in the model: the callers settle or move aside an untrusted
// repository first, and take git's reference lock for an existing one.
func TestDropMarkerGuards(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)
	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))

	st := d.lockState(f.path)
	defer st.file.Unlock()

	d.mu.Lock()
	st.untrusted = errors.New("input/output error")
	d.mu.Unlock()
	dropped, err := d.dropMarker(f.path, st, true)
	require.NoError(t, err)
	require.False(t, dropped, "an untrusted repository keeps its marker")

	d.mu.Lock()
	st.untrusted = nil
	d.mu.Unlock()
	dropped, err = d.dropMarker(f.path, st, false)
	require.NoError(t, err)
	require.False(t, dropped, "the marker of an existing repository goes only under git's reference lock")
	require.FileExists(t, f.markerPath(t, d))

	dropped, err = d.dropMarker(f.path, st, true)
	require.NoError(t, err)
	require.True(t, dropped)
	require.NoFileExists(t, f.markerPath(t, d))
}

// TestMovedRepositoryKeepsTheMarkerOfALiveProcess is the fourth review's
// sequence: a repository is moved aside while a git process that marked it
// still runs, the marker stays through a durable step of the missing path and
// through the hydration, so the process's later write is covered; it goes once
// the process exited and a step made the stack durable.
func TestMovedRepositoryKeepsTheMarkerOfALiveProcess(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)
	template := filepath.Join(t.TempDir(), "template.git")
	copyTree(t, f.path, template)

	end := d.Begin(f.path)
	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, os.Rename(f.path, filepath.Join(t.TempDir(), "aside")))
	require.NoError(t, d.Forget(f.ctx, f.repo, f.path))
	require.FileExists(t, f.markerPath(t, d))

	require.NoError(t, d.Settle(f.path, true))
	require.FileExists(t, f.markerPath(t, d), "the process names the path: a hydration there gets its writes")

	copyTree(t, template, f.path)
	require.NoError(t, d.Settle(f.path, true))
	require.FileExists(t, f.markerPath(t, d))

	end()
	require.NoError(t, d.Settle(f.path, true))
	require.NoFileExists(t, f.markerPath(t, d))
	require.Empty(t, d.Tracked())
}

// TestRecoveredMarkerOfAMissingRepositoryIsSettled verifies that a marker found
// at the recovery for a repository that no longer exists is removed by the
// next durable step, instead of failing on its missing stack at every step.
func TestRecoveredMarkerOfAMissingRepositoryIsSettled(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)

	old := f.durability(t, "boot-a", 0)
	require.NoError(t, old.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, old.Close())
	require.NoError(t, os.Rename(f.path, filepath.Join(t.TempDir(), "aside")))

	next := f.durability(t, "boot-a", 0)
	_, err := next.Recover(f.ctx, []string{f.repo.Storage})
	require.NoError(t, err)
	require.NoError(t, next.Settle(f.path, false))
	require.NoFileExists(t, f.markerPath(t, next))
	require.Empty(t, next.Tracked())
}

// TestGitActivityPushesTheGraceForward verifies that the recovery grace counts
// from the last git activity seen in the stack, not from the recovery: a git
// process of the previous owner may start its auto-compaction right after its
// commit.
func TestGitActivityPushesTheGraceForward(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)

	old := f.durability(t, "boot-a", 0)
	require.NoError(t, old.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, old.Close())

	next := f.durability(t, "boot-a", 0, WithRecoveryGrace(time.Hour))
	_, err := next.Recover(f.ctx, []string{f.repo.Storage})
	require.NoError(t, err)
	// The grace of the recovery has passed.
	next.mu.Lock()
	next.repos[f.path].settleAfter = time.Now().Add(-time.Second)
	next.mu.Unlock()

	temp := filepath.Join(f.path, "reftable", "0x000000000009-0x000000000009-abcdef01.ref.temp.XyZ123")
	require.NoError(t, os.WriteFile(temp, nil, 0o666))
	before := time.Now()
	require.NoError(t, next.Settle(f.path, true))
	require.FileExists(t, f.markerPath(t, next))

	require.NoError(t, os.Remove(temp))
	require.NoError(t, next.Settle(f.path, true))
	require.FileExists(t, f.markerPath(t, next), "the grace counts from the activity")
	next.mu.Lock()
	require.True(t, next.repos[f.path].settleAfter.After(before.Add(time.Hour-time.Minute)))
	next.mu.Unlock()
}

// TestStaleGitLockDoesNotPinTheMarker verifies git's lock files left by a git
// process that died: a lock of the table list or a temporary table older than
// housekeeping's grace is not activity, and the durable step removes such a
// list lock as housekeeping would, takes its own and settles. A younger one
// keeps the marker.
func TestStaleGitLockDoesNotPinTheMarker(t *testing.T) {
	t.Parallel()
	require.Equal(t, housekeeping.ReftableLockfileGracePeriod, staleGitFileAge, "the rule of Gitaly's housekeeping")

	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)
	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))

	lock := filepath.Join(f.path, "reftable", "tables.list.lock")
	temp := filepath.Join(f.path, "reftable", "0x000000000009-0x000000000009-abcdef01.ref.temp.XyZ123")
	for _, file := range []string{lock, temp} {
		require.NoError(t, os.WriteFile(file, nil, 0o666))
	}
	require.NoError(t, d.Settle(f.path, true))
	require.FileExists(t, f.markerPath(t, d), "a git process may hold them")

	old := time.Now().Add(-staleGitFileAge - time.Minute)
	for _, file := range []string{lock, temp} {
		require.NoError(t, os.Chtimes(file, old, old))
	}
	require.NoError(t, d.Settle(f.path, true))
	require.NoFileExists(t, f.markerPath(t, d))
	require.NoFileExists(t, lock, "removed as housekeeping would, and the step's own released")
}

// TestRepositoryWithoutAStackHasNothingToMakeDurable verifies a repository
// that is not reftable (a files-backend copy being imported at the path): it
// has no stack, so its marker is settled and handed over without one, and the
// recovery of a kernel restart does not fail on it.
func TestRepositoryWithoutAStackHasNothingToMakeDurable(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	ctx := f.ctx
	pb, path := gittest.CreateRepository(t, ctx, f.cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
		ReferenceBackend:       git.ReferenceBackendFiles,
	})
	repo := RepoRef{Storage: pb.GetStorageName(), RelativePath: pb.GetRelativePath()}
	markerPath := func(d *Durability) string {
		p, err := d.markerPath(ctx, repo, path)
		require.NoError(t, err)
		return p
	}

	d := f.durability(t, "boot-a", 0)
	require.NoError(t, d.MarkDirty(ctx, repo, path))
	require.NoError(t, d.Settle(path, false))
	require.NoFileExists(t, markerPath(d))

	require.NoError(t, d.MarkDirty(ctx, repo, path))
	require.NoError(t, d.Close())
	next := f.durability(t, "boot-b", 0)
	found, err := next.Recover(ctx, []string{repo.Storage})
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.True(t, found[0].Reboot)
	require.NoError(t, next.Forget(ctx, repo, path), "kept in place: not a cache of the storage core")
	require.NoFileExists(t, markerPath(next))
	require.Empty(t, next.Tracked())
}

// modelProc is a git process of the model.
type modelProc struct {
	end    func()
	marked bool
	// orphan is a process of an owner that restarted: it is not counted by the
	// running durability, and it only finishes the write it was in (inFlight,
	// the temporary table it writes).
	orphan   bool
	inFlight string
}

type markerModel struct {
	t    *testing.T
	f    durabilityFixture
	rng  *rand.Rand
	d    *Durability
	boot int

	template string
	asideDir string
	reftable string

	procs []*modelProc
	// pending are the tables at the path written by the model's git processes
	// and not made durable since; listPending and dirPending the table list and
	// the directory entries.
	pending     map[string]bool
	listPending bool
	dirPending  bool
	// untrusted is set by a failed fsync and by a kernel restart with a marker,
	// rebootUntrusted only by the latter (the repository may then be accepted in
	// place).
	untrusted       bool
	rebootUntrusted bool
	// failNext makes the fsyncs of the stack fail during the next durable step.
	failNext bool
	failing  bool
	tables   int
	moves    int
}

func runMarkerModel(t *testing.T, seed int64, steps int) {
	f := setupDurability(t)
	m := &markerModel{
		t:        t,
		f:        f,
		rng:      rand.New(rand.NewSource(seed)),
		template: filepath.Join(t.TempDir(), "template.git"),
		asideDir: t.TempDir(),
		reftable: filepath.Join(f.path, "reftable"),
		pending:  map[string]bool{},
	}
	copyTree(t, f.path, m.template)
	m.d = m.newDurability()

	for step := 0; step < steps; step++ {
		op := m.step()
		m.check(fmt.Sprintf("seed %d step %d (%s)", seed, step, op))
	}
}

// sync is the durability's fsync: it fails the stack's files while failing is
// set, and tells the model which files are durable.
func (m *markerModel) sync(file *os.File) error {
	name := file.Name()
	inStack := name == m.reftable || filepath.Dir(name) == m.reftable
	if m.failing && inStack {
		return errors.New("input/output error")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	switch {
	case name == m.reftable:
		m.dirPending = false
	case !inStack:
	case filepath.Base(name) == "tables.list":
		m.listPending = false
	default:
		delete(m.pending, filepath.Base(name))
	}
	return nil
}

func (m *markerModel) newDurability() *Durability {
	m.boot++
	d, err := NewDurability(m.f.locator, 0, WithBootID(fmt.Sprintf("boot-%d", m.boot)), WithSync(m.sync), WithRecoveryGrace(0))
	require.NoError(m.t, err)
	m.t.Cleanup(func() { require.NoError(m.t, d.Close()) })
	m.f.own(m.t, d)
	_, err = d.Recover(m.f.ctx, []string{m.f.repo.Storage})
	require.NoError(m.t, err)
	d.Activate()
	return d
}

func (m *markerModel) exists() bool {
	_, err := os.Lstat(m.f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	require.NoError(m.t, err)
	return true
}

func (m *markerModel) live(match func(*modelProc) bool) []*modelProc {
	var procs []*modelProc
	for _, p := range m.procs {
		if match(p) {
			procs = append(procs, p)
		}
	}
	return procs
}

func (m *markerModel) remove(p *modelProc) {
	for i, q := range m.procs {
		if q == p {
			m.procs = append(m.procs[:i], m.procs[i+1:]...)
			return
		}
	}
}

func (m *markerModel) pick(procs []*modelProc) *modelProc {
	return procs[m.rng.Intn(len(procs))]
}

// step runs one random operation and returns its name.
func (m *markerModel) step() string {
	own := func(p *modelProc) bool { return !p.orphan }
	switch m.rng.Intn(14) {
	case 0, 1:
		if len(m.procs) < 4 {
			m.procs = append(m.procs, &modelProc{end: m.d.Begin(m.f.path)})
			return "begin"
		}
	case 2:
		unmarked := m.live(func(p *modelProc) bool { return own(p) && !p.marked })
		if len(unmarked) > 0 && m.exists() {
			require.NoError(m.t, m.d.MarkDirty(m.f.ctx, m.f.repo, m.f.path))
			m.pick(unmarked).marked = true
			return "mark"
		}
	case 3, 4:
		writers := m.live(func(p *modelProc) bool { return own(p) && p.marked })
		if len(writers) > 0 && m.exists() {
			m.writeTable(m.nextTable(), "")
			return "write"
		}
	case 5:
		procs := m.live(own)
		if len(procs) > 0 {
			p := m.pick(procs)
			p.end()
			m.remove(p)
			return "end"
		}
	case 6, 7:
		m.failing = m.failNext
		m.failNext = false
		err := m.d.Settle(m.f.path, m.rng.Intn(2) == 0)
		m.failing = false
		if errors.Is(err, ErrUntrusted) {
			m.untrusted = true
		} else {
			require.NoError(m.t, err)
		}
		return "settle"
	case 8:
		m.failNext = true
		return "fail the next fsyncs"
	case 9:
		if m.exists() {
			m.moves++
			require.NoError(m.t, os.Rename(m.f.path, filepath.Join(m.asideDir, fmt.Sprint(m.moves))))
			require.NoError(m.t, m.d.Forget(m.f.ctx, m.f.repo, m.f.path))
			m.pending = map[string]bool{}
			m.listPending, m.dirPending = false, false
			m.untrusted, m.rebootUntrusted = false, false
			return "move aside"
		}
	case 10:
		if !m.exists() {
			// A hydration writes the repository durably before it moves it in.
			copyTree(m.t, m.template, m.f.path)
			return "hydrate"
		}
	case 11:
		return m.restart()
	case 12:
		if orphans := m.live(func(p *modelProc) bool { return p.orphan }); len(orphans) > 0 {
			p := m.pick(orphans)
			if m.exists() && m.rng.Intn(4) != 0 {
				m.writeTable(strings.TrimSuffix(filepath.Base(p.inFlight), ".temp.XyZ123"), p.inFlight)
			}
			// Otherwise the process is killed: its temporary table stays.
			m.remove(p)
			return "orphan exits"
		}
	case 13:
		if m.rebootUntrusted && m.exists() {
			// The repository is not a cache of the storage core: kept in place.
			require.NoError(m.t, m.d.Forget(m.f.ctx, m.f.repo, m.f.path))
			m.pending = map[string]bool{}
			m.listPending, m.dirPending = false, false
			m.untrusted, m.rebootUntrusted = false, false
			return "accept in place"
		}
		return m.reboot()
	}
	return "nothing"
}

func (m *markerModel) nextTable() string {
	m.tables++
	return fmt.Sprintf("0x%012x-0x%012x-model%04d.ref", 100+m.tables, 100+m.tables, m.tables)
}

// writeTable writes a table and lists it, as git does, without fsync. A table
// written through a temporary file (temp) is renamed into place.
func (m *markerModel) writeTable(name, temp string) {
	table := filepath.Join(m.reftable, name)
	if temp != "" {
		if err := os.Rename(temp, table); errors.Is(err, fs.ErrNotExist) {
			// The repository it was writing was moved aside meanwhile.
			return
		} else {
			require.NoError(m.t, err)
		}
	} else {
		require.NoError(m.t, os.WriteFile(table, []byte(name), 0o666))
	}
	list := filepath.Join(m.reftable, "tables.list")
	content, err := os.ReadFile(list)
	require.NoError(m.t, err)
	require.NoError(m.t, os.WriteFile(list+".model", append(content, []byte(name+"\n")...), 0o666))
	require.NoError(m.t, os.Rename(list+".model", list))
	m.pending[name] = true
	m.listPending, m.dirPending = true, true
}

// restart restarts the process in the same boot: its git processes that may
// write become orphans, half of them in the middle of a table write.
func (m *markerModel) restart() string {
	require.NoError(m.t, m.d.Close())
	var orphans []*modelProc
	for _, p := range m.procs {
		if p.orphan || !p.marked || !m.exists() || m.rng.Intn(2) == 0 {
			continue
		}
		temp := filepath.Join(m.reftable, m.nextTable()+".temp.XyZ123")
		require.NoError(m.t, os.WriteFile(temp, []byte("in flight"), 0o666))
		orphans = append(orphans, &modelProc{orphan: true, inFlight: temp})
	}
	m.procs = orphans
	m.boot--
	m.d = m.newDurability()
	return "restart"
}

// reboot restarts the kernel: every process dies, and a repository with a
// marker becomes untrusted (its writes may never have reached the disk).
func (m *markerModel) reboot() string {
	require.NoError(m.t, m.d.Close())
	_, err := os.Stat(m.f.markerPath(m.t, m.d))
	marked := err == nil
	m.procs = nil
	if marked {
		m.untrusted = true
		m.rebootUntrusted = true
	}
	m.d = m.newDurability()
	return "reboot"
}

// check asserts the invariants.
func (m *markerModel) check(where string) {
	_, err := os.Stat(m.f.markerPath(m.t, m.d))
	markerOnDisk := err == nil

	// A process of the previous owner may still write only while its temporary
	// table is there: git renames it into the stack by its path.
	mayWrite := len(m.live(func(p *modelProc) bool {
		if p.orphan {
			_, err := os.Stat(p.inFlight)
			return err == nil
		}
		return p.marked
	})) > 0
	unsynced := m.exists() && (len(m.pending) > 0 || m.listPending || m.dirPending)
	if mayWrite || unsynced {
		require.True(m.t, markerOnDisk, "%s: a write may not be durable (writers %t, pending %v, list %t, dir %t) but the marker is gone",
			where, mayWrite, m.pending, m.listPending, m.dirPending)
	}
	require.Equal(m.t, m.untrusted, m.d.Untrusted(m.f.path), "%s: untrusted", where)
	if m.untrusted {
		require.True(m.t, markerOnDisk, "%s: an untrusted repository lost its marker", where)
	}
}

// copyTree copies the directory src to dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o777)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o666)
	}))
}
