package localrefs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// MarkerDirName is the directory of the dirty markers in a storage, under
// Gitaly's data directory. It is not the state directory, whose files the disk
// cache walker removes after an hour.
const MarkerDirName = config.GitalyDataPrefix + "/graft-ref-durability"

// ownerLockName is the file in MarkerDirName whose exclusive lock makes a
// process the owner of the storage's deferred durability.
const ownerLockName = "owner.lock"

// markerTempPrefix prefixes the temporary files of marker writes.
const markerTempPrefix = ".marker-"

// stepTempPrefix prefixes the temporary files the durable step links its
// reference lock from, in the repository directory.
const stepTempPrefix = ".graft-durable-step-"

// bootIDPath is where Linux exposes the identity of the running kernel boot.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

const (
	// DurableStepInterval is how often the durable step runs for every dirty
	// repository: it bounds how long a write failure the kernel reports goes
	// unnoticed. A process that does not own the deferred durability tries to
	// take it as often.
	DurableStepInterval = time.Second
	// DurableStepIdle is how long a dirty repository must be without a reference
	// write before the durable step removes its marker.
	DurableStepIdle = time.Second
	// RecoveryGrace is how long a repository found dirty when the process takes
	// the ownership keeps its marker at least. The rule that decides is that no
	// git lock or temporary file is in its reftable directory (gitActive); the
	// grace only covers the instant between a commit's rename and the start of
	// its auto-compaction, when a git process has neither.
	RecoveryGrace = 2 * time.Second
)

// WithRecoveryGrace replaces RecoveryGrace. Tests shorten it.
func WithRecoveryGrace(grace time.Duration) DurabilityOption {
	return func(d *Durability) { d.recoveryGrace = grace }
}

// CrashEnv names the test-only environment variable that makes the process
// exit (exit code 75) in the durable step, while it holds git's reference lock
// after making the stack durable and before it removes the marker: "durable_step".
// The crash tests with real binaries use it. It is read once by NewDurability.
const CrashEnv = "GRAFT_STORAGE_TEST_CRASH"

const (
	crashDurableStep = "durable_step"
	crashExitCode    = 75
)

// stackLockTag starts the content of the reference lock the durable step
// takes, followed by a random nonce of that lock. git never writes it (a lock
// of git holds table names), so a lock with this tag found by a new owner was
// left by a durable step that died, and a step removes a lock only when it
// still holds its own nonce.
const stackLockTag = "graft durable step "

// ErrUntrusted is returned by Settle when the kernel reported that making the
// references of a repository durable failed: it may have dropped some of their
// writes, so the local repository must not be served before it is rebuilt.
var ErrUntrusted = errors.New("storage core: the local references may have lost writes")

// UntrustedError is ErrUntrusted for one repository.
type UntrustedError struct {
	Repo RepoRef
	Path string
	Err  error
}

func (e *UntrustedError) Error() string {
	return fmt.Sprintf("%v: %s: %v", ErrUntrusted, e.Path, e.Err)
}

// Unwrap returns ErrUntrusted and the cause.
func (e *UntrustedError) Unwrap() []error { return []error{ErrUntrusted, e.Err} }

// writebackError is an fsync that failed: the kernel reports a write it could
// not make durable. Every other error of the durable step (opening a file,
// reading the table list) is transient and retried at the next step.
type writebackError struct{ err error }

func (e writebackError) Error() string { return "fsync: " + e.err.Error() }
func (e writebackError) Unwrap() error { return e.err }

// Durability defers the durability of the local reference writes of the
// storage core in authoritative mode (doc/graft/PLAN7.md, LOCAL REFERENCE
// WRITES RULE). The database commit of a transaction is its durable commit
// point and the local repository is a cache of it, so git writes the references
// without fsync and Durability keeps the cache safe to serve:
//
//   - one process per storage owns the deferred durability (an exclusive lock
//     held for the life of the process); only the owner writes references
//     without fsync, runs the durable steps and touches the durable step's
//     locks. Another process on the same storage (a graceful upgrade overlaps
//     the old and the new process) writes them with fsync until it owns;
//   - before the first reference write that is not durable, the repository
//     gets a dirty marker, written durably, that holds the identity of the
//     kernel boot (MarkDirty);
//   - a durable step makes the references durable and removes the marker,
//     under git's reference lock and only when no transaction of the process is
//     live (Settle);
//   - when it takes the ownership, a marker of another boot means the kernel
//     restarted while the repository had writes that may never have reached the
//     disk: its reftable stack may be torn, which git reads as an empty
//     repository, so the repository is moved aside and hydrated again
//     (Recover). A marker of the running boot is a process restart: the kernel
//     kept every write.
//
// A nil *Durability is off: every reference write is durable by itself
// (core.fsync with references), as outside authoritative mode.
type Durability struct {
	bootID  string
	locator storage.Locator
	// idle is how long a dirty repository must be without a reference write
	// before Settle removes its marker.
	idle time.Duration
	// sync makes a file or directory durable; tests replace it.
	sync func(f *os.File) error
	// crashAt is the crash point selected with CrashEnv.
	crashAt string

	// ownerFiles hold the owner locks, taken by TryOwn. owner is set by
	// Activate once the process has recovered the storages: from then on its
	// writes are deferred. Neither goes back.
	owner      atomic.Bool
	ownerMu    sync.Mutex
	ownerFiles []*os.File
	locked     bool
	// recoveryGrace is how long a repository recovered dirty keeps its marker
	// at least: a git process of the previous owner that outlived it (killed
	// with it, but not reaped yet) may still be committing for that long.
	recoveryGrace time.Duration

	mu sync.Mutex
	// repos holds the state of every repository written since the process
	// started (one entry per repository; Forget drops it).
	repos map[string]*refState

	marked     prometheus.Counter
	cleared    prometheus.Counter
	untrusted  *prometheus.CounterVec
	stepErrors prometheus.Counter
	dirty      prometheus.Gauge
	inflight   prometheus.Gauge
	owning     prometheus.Gauge
}

var _ prometheus.Collector = (*Durability)(nil)

// refState is what Durability knows of one local repository, by its path. Its
// fields are guarded by Durability.mu, except marker writes and removals,
// which file serializes.
type refState struct {
	// file serializes the writes and the removal of the marker file.
	file   sync.Mutex
	repo   RepoRef
	marker string
	dirty  bool
	// nLive counts the git processes of this process that write the
	// repository's references without fsync and have not exited: any of them
	// may still write a table or compact the stack.
	nLive     int
	lastWrite time.Time
	// recovered is set for a repository found dirty when the process took the
	// ownership: a git process of the previous owner that outlived it may still
	// be writing, so its marker stays while git's lock or temporary files are
	// there (gitActive), and not before settleAfter.
	recovered   bool
	settleAfter time.Time
	// untrusted is the first failed fsync of the repository. The kernel reports
	// a failed writeback once, so a later fsync can succeed although writes
	// were lost: the repository never becomes clean again, only moved aside.
	// The marker records it, so that a restart of the process keeps it.
	untrusted error
	// synced holds the tables made durable during the current dirty period that
	// the stack still lists. Tables are immutable, so one fsync each is enough.
	synced map[string]struct{}
}

// RepoRef names a repository of a storage.
type RepoRef struct {
	Storage      string `json:"storage"`
	RelativePath string `json:"relative_path"`
}

func repoOf(r RepoRef) *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: r.Storage, RelativePath: r.RelativePath}
}

// marker is the content of a dirty marker.
type marker struct {
	RepoRef
	BootID string `json:"boot_id"`
	// Untrusted is the failed fsync that made the repository untrusted, empty
	// while it is only dirty.
	Untrusted string `json:"untrusted,omitempty"`
}

// Reasons of graft_storage_ref_untrusted_total.
const (
	// ReasonReboot is a dirty repository found after a kernel restart.
	ReasonReboot = "reboot"
	// ReasonSyncFailed is a durable step whose fsync failed.
	ReasonSyncFailed = "sync_failed"
)

// DurabilityOption configures a Durability.
type DurabilityOption func(*Durability)

// WithBootID sets the identity of the kernel boot instead of reading it from
// the kernel. Tests use it to simulate a kernel restart.
func WithBootID(bootID string) DurabilityOption {
	return func(d *Durability) { d.bootID = bootID }
}

// WithSync replaces how a file or directory is made durable. Tests use it to
// simulate a write the kernel failed to do.
func WithSync(sync func(f *os.File) error) DurabilityOption {
	return func(d *Durability) { d.sync = sync }
}

// NewDurability returns the deferred durability of the node, or nil when the
// kernel does not expose its boot identity (not Linux) and no option sets one:
// the references are then written durably by every transaction.
func NewDurability(locator storage.Locator, idle time.Duration, opts ...DurabilityOption) (*Durability, error) {
	d := newDurability(locator, idle)
	for _, opt := range opts {
		opt(d)
	}
	if d.bootID != "" {
		return d, nil
	}

	content, err := os.ReadFile(bootIDPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the boot identity: %w", err)
	}
	d.bootID = strings.TrimSpace(string(content))
	if d.bootID == "" {
		return nil, fmt.Errorf("read the boot identity: %s is empty", bootIDPath)
	}
	return d, nil
}

func newDurability(locator storage.Locator, idle time.Duration) *Durability {
	return &Durability{
		locator:       locator,
		idle:          idle,
		sync:          (*os.File).Sync,
		crashAt:       os.Getenv(CrashEnv),
		recoveryGrace: RecoveryGrace,
		repos:         map[string]*refState{},
		marked: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_ref_dirty_marked_total",
			Help: "Number of dirty markers written: a repository got local reference writes that are not durable yet.",
		}),
		cleared: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_ref_dirty_cleared_total",
			Help: "Number of dirty markers removed by a durable step.",
		}),
		untrusted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graft_storage_ref_untrusted_total",
			Help: "Number of local repositories whose references could not be trusted and were moved aside to be hydrated again, by reason (reboot, sync_failed).",
		}, []string{"reason"}),
		stepErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_ref_durable_step_errors_total",
			Help: "Number of durable steps that failed with a transient error (not a failed fsync) and are retried at the next step.",
		}),
		dirty: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "graft_storage_ref_dirty_repositories",
			Help: "Number of local repositories with reference writes that are not durable yet.",
		}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "graft_storage_ref_writes_inflight",
			Help: "Number of git processes of this process that write references without fsync and have not exited; their repositories keep their markers.",
		}),
		owning: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "graft_storage_ref_durability_owner",
			Help: "1 when this process owns the deferred durability of its storages and writes references without fsync, 0 while another process owns it.",
		}),
	}
}

// Enabled reports whether the node defers the durability of its reference
// writes once it owns it.
func (d *Durability) Enabled() bool { return d != nil }

// Deferring reports whether this process owns the deferred durability, so its
// reference writes are made without fsync. It goes from false to true at most
// once.
func (d *Durability) Deferring() bool { return d != nil && d.owner.Load() }

// TryOwn takes the owner locks of the storages without waiting. It returns
// false when another process holds one of them; the locks taken are kept, and
// a later call takes the rest. Owning the locks lets the process recover the
// storages (Recover); its writes are deferred only after Activate, so that no
// writer of this process writes a marker that the recovery could take for one
// left behind.
func (d *Durability) TryOwn(ctx context.Context, storages []string) (bool, error) {
	if d == nil {
		return false, nil
	}

	d.ownerMu.Lock()
	defer d.ownerMu.Unlock()
	if d.locked {
		return true, nil
	}

	for _, storageName := range storages[len(d.ownerFiles):] {
		dir, err := d.markerDir(ctx, storageName)
		if err != nil {
			return false, err
		}
		if err := d.ensureDir(dir); err != nil {
			return false, fmt.Errorf("create the dirty marker directory: %w", err)
		}
		f, err := os.OpenFile(filepath.Join(dir, ownerLockName), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return false, fmt.Errorf("open the owner lock: %w", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return false, nil
			}
			return false, fmt.Errorf("lock the owner lock: %w", err)
		}
		d.ownerFiles = append(d.ownerFiles, f)
	}

	d.locked = true
	return true, nil
}

// Activate makes the writes of the process deferred; it holds the owner locks
// (TryOwn) and has recovered the storages.
func (d *Durability) Activate() {
	if d == nil {
		return
	}

	d.ownerMu.Lock()
	defer d.ownerMu.Unlock()
	if !d.locked {
		panic("localrefs: Activate without the owner locks")
	}
	d.owner.Store(true)
	d.owning.Set(1)
}

// ownsLocks reports whether the process holds the owner locks.
func (d *Durability) ownsLocks() bool {
	d.ownerMu.Lock()
	defer d.ownerMu.Unlock()
	return d.locked
}

// Close releases the ownership. The process must not write references
// without fsync afterwards; only tests and the end of the process call it.
func (d *Durability) Close() error {
	if d == nil {
		return nil
	}

	d.ownerMu.Lock()
	defer d.ownerMu.Unlock()
	var errs []error
	for _, f := range d.ownerFiles {
		errs = append(errs, f.Close())
	}
	d.ownerFiles = nil
	d.locked = false
	return errors.Join(errs...)
}

// markerPath returns the path of the dirty marker of the repository at path.
func (d *Durability) markerPath(ctx context.Context, repo RepoRef, path string) (string, error) {
	dir, err := d.markerDir(ctx, repo.Storage)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), nil
}

func (d *Durability) markerDir(ctx context.Context, storageName string) (string, error) {
	root, err := d.locator.GetStorageByName(ctx, storageName)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, MarkerDirName), nil
}

// stateLocked returns the state of the repository at path, creating it. The
// caller holds d.mu.
func (d *Durability) stateLocked(path string) *refState {
	st := d.repos[path]
	if st == nil {
		st = &refState{synced: map[string]struct{}{}}
		d.repos[path] = st
	}
	return st
}

// MarkDirty records that the repository at path is about to get reference
// writes that are not durable. When it returns nil the repository has a durable
// dirty marker; the writer must not write before. It is only called while the
// process owns the deferred durability (Deferring). The caller holds git's
// reference lock of the repository (the voter's Prepared phase) or the syncer's
// repository lock, under which Settle decides to clear the marker, and the
// marker file is only written or removed under the state's file lock, so a
// marker is never removed after a writer relied on it.
func (d *Durability) MarkDirty(ctx context.Context, repo RepoRef, path string) error {
	if d == nil {
		return nil
	}

	// The state is the map's after its file lock is taken: a durable step may
	// have dropped a clean state meanwhile.
	var st *refState
	for {
		d.mu.Lock()
		st = d.stateLocked(path)
		d.mu.Unlock()
		st.file.Lock()
		d.mu.Lock()
		current := d.repos[path] == st
		if current {
			st.lastWrite = time.Now()
		}
		dirty := st.dirty
		d.mu.Unlock()
		if !current {
			st.file.Unlock()
			continue
		}
		if dirty {
			st.file.Unlock()
			return nil
		}
		break
	}
	defer st.file.Unlock()

	markerPath, err := d.markerPath(ctx, repo, path)
	if err != nil {
		return err
	}
	content, err := json.Marshal(marker{RepoRef: repo, BootID: d.bootID})
	if err != nil {
		return fmt.Errorf("encode the dirty marker: %w", err)
	}
	if err := d.writeDurably(markerPath, content); err != nil {
		return fmt.Errorf("write the dirty marker: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	st.repo = repo
	st.marker = markerPath
	d.marked.Inc()
	if !st.dirty {
		st.dirty = true
		d.dirty.Inc()
	}
	return nil
}

// Begin records that a git process that writes the references of the
// repository at path without fsync is about to start; the caller calls end
// once the process has exited. Until then the process may write a table or
// compact the stack, whatever hooks git runs or skips (a failed prepare, a
// failed commit, a kill), so the durable step keeps the marker. end may be
// called more than once.
func (d *Durability) Begin(path string) (end func()) {
	if d == nil {
		return func() {}
	}

	d.mu.Lock()
	st := d.stateLocked(path)
	st.nLive++
	d.inflight.Inc()
	d.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			// Forget may have dropped the state; the count belongs to it.
			if d.repos[path] == st {
				st.nLive--
				d.inflight.Dec()
			}
		})
	}
}

// Dirty returns the paths of the dirty repositories.
func (d *Durability) Dirty() []string {
	if d == nil {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	paths := make([]string, 0, len(d.repos))
	for path, st := range d.repos {
		if st.dirty {
			paths = append(paths, path)
		}
	}
	return paths
}

// RepoOf returns the repository whose state is kept for path.
func (d *Durability) RepoOf(path string) (RepoRef, bool) {
	if d == nil {
		return RepoRef{}, false
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.repos[path]
	if st == nil || st.repo == (RepoRef{}) {
		return RepoRef{}, false
	}
	return st.repo, true
}

// CountStepError counts a durable step that failed with a transient error.
func (d *Durability) CountStepError() {
	if d == nil {
		return
	}
	d.stepErrors.Inc()
}

// Settle is the durable step of the repository at path; only the owner runs
// it. It makes the tables that the stack lists, the list and the directory
// durable. A failed fsync is an *UntrustedError (the caller moves the
// repository aside at once); any other error is transient. When the repository
// has been idle for the idle period, or final is set (the process stops), and
// no transaction of the process is live, it then removes the marker: the
// decision is taken under git's reference lock, taken without waiting (a busy
// lock leaves the marker for the next step), and the marker file is removed
// after the lock is released, under the state's file lock that a writer's
// MarkDirty waits for. A repository that no longer exists (moved aside as a
// stale cache) loses its marker. The caller holds the syncer's repository lock,
// so no syncer write and no replacement of the repository runs meanwhile.
func (d *Durability) Settle(path string, final bool) error {
	if !d.Deferring() {
		return nil
	}

	d.mu.Lock()
	st := d.repos[path]
	if st == nil || !st.dirty {
		d.mu.Unlock()
		return nil
	}
	repo := st.repo
	if st.untrusted != nil {
		// Sticky: the move aside is retried, the marker never cleared.
		untrusted := st.untrusted
		d.mu.Unlock()
		return &UntrustedError{Repo: repo, Path: path, Err: untrusted}
	}
	now := time.Now()
	idle := st.nLive == 0 && now.After(st.settleAfter) && (final || now.Sub(st.lastWrite) >= d.idle)
	recovered := st.recovered
	d.mu.Unlock()
	if idle && recovered {
		// A git process of the previous owner may still be writing.
		active, err := gitActive(path)
		if err != nil {
			return err
		}
		idle = !active
	}

	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		st.file.Lock()
		defer st.file.Unlock()
		d.clear(st)
		if err := d.removeMarker(st); err != nil {
			return err
		}
		d.prune(path, st)
		return nil
	}

	if err := d.syncStack(path, st); err != nil {
		return d.stepError(st, repo, path, err)
	}
	if !idle {
		return nil
	}

	lock, ok, err := lockStack(path)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	cleared, err := d.settleLocked(path, st)
	if err != nil {
		_ = lock.release()
		return d.stepError(st, repo, path, err)
	}
	if !cleared {
		return lock.release()
	}

	// The state is clean from here on, and a writer that takes git's lock now
	// writes a new marker: it waits for the file lock, held until the old
	// marker is removed.
	defer st.file.Unlock()
	if err := lock.release(); err != nil {
		return err
	}
	if err := d.removeMarker(st); err != nil {
		return err
	}
	d.prune(path, st)
	return nil
}

// prune drops the state of a repository that is clean and has no live git
// process, so that the states do not grow with every repository ever written.
// The caller holds the state's file lock: MarkDirty takes the state again from
// the map after its own.
func (d *Durability) prune(path string, st *refState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.repos[path] == st && !st.dirty && st.untrusted == nil && st.nLive == 0 {
		delete(d.repos, path)
	}
}

// stepError is the error of a durable step: an *UntrustedError for a failed
// fsync, which makes the repository untrusted for good, the error itself
// otherwise. The first failure is written into the marker, so that the
// repository stays untrusted across a restart of the process (the next fsync
// would succeed); when even that write fails, the disk is failing and the
// state of this process keeps it untrusted.
func (d *Durability) stepError(st *refState, repo RepoRef, path string, err error) error {
	var writeback writebackError
	if !errors.As(err, &writeback) {
		return err
	}

	d.mu.Lock()
	first := st.untrusted == nil
	if first {
		st.untrusted = err
	}
	markerPath := st.marker
	d.mu.Unlock()

	if first && markerPath != "" {
		st.file.Lock()
		content, encodeErr := json.Marshal(marker{RepoRef: repo, BootID: d.bootID, Untrusted: err.Error()})
		if encodeErr == nil {
			encodeErr = d.writeDurably(markerPath, content)
		}
		st.file.Unlock()
		if encodeErr != nil {
			return &UntrustedError{Repo: repo, Path: path, Err: fmt.Errorf("%w (recording it in the marker failed: %w)", err, encodeErr)}
		}
	}
	return &UntrustedError{Repo: repo, Path: path, Err: err}
}

// gitActive reports whether a git process may be writing the reftable stack of
// the repository at path: git's lock of the list, a lock of a table being
// compacted, or a table being written is there.
func gitActive(path string) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(path, "reftable"))
	if err != nil {
		return false, fmt.Errorf("read the reftable directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".lock") || strings.Contains(name, ".temp.") {
			return true, nil
		}
	}
	return false, nil
}

// settleLocked makes the tables written since the first pass durable and, when
// no transaction of the process is live, marks the state clean with its file
// lock held (the caller releases it). The caller holds git's reference lock: no
// transaction can take it meanwhile, and the live transactions are counted from
// before they take it until their end.
func (d *Durability) settleLocked(path string, st *refState) (bool, error) {
	d.mu.Lock()
	busy := st.nLive > 0 || st.untrusted != nil
	d.mu.Unlock()
	if busy {
		return false, nil
	}

	if err := d.syncStack(path, st); err != nil {
		return false, err
	}
	if d.crashAt == crashDurableStep {
		os.Exit(crashExitCode)
	}

	st.file.Lock()
	d.clear(st)
	return true, nil
}

// clear marks the state clean. The caller holds the state's file lock.
func (d *Durability) clear(st *refState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st.dirty {
		st.dirty = false
		d.cleared.Inc()
		d.dirty.Dec()
	}
	st.synced = map[string]struct{}{}
}

// removeMarker removes the marker file of a clean state. The caller holds the
// state's file lock.
func (d *Durability) removeMarker(st *refState) error {
	if st.marker == "" {
		return nil
	}
	if err := os.Remove(st.marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the dirty marker: %w", err)
	}
	return nil
}

// stackLock is git's reference lock of a repository, taken by the durable
// step.
type stackLock struct {
	path string
	// content is the tag and the nonce the step wrote into it, and info the file
	// it created: a lock is the step's own only when both still match (an inode
	// is reused as soon as it is freed, so the file alone does not tell).
	content []byte
	info    os.FileInfo
}

// lockStack takes git's reference lock of the repository at path without
// waiting: ok is false when it is held. The lock is created with its content
// at once (a linked temporary file), so that a lock left by a process that
// died is recognized by the next owner (removeStaleStackLock).
func lockStack(path string) (*stackLock, bool, error) {
	lockPath := filepath.Join(path, "reftable", "tables.list.lock")

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}
	content := []byte(stackLockTag + hex.EncodeToString(nonce) + "\n")

	tmp, err := os.CreateTemp(path, stepTempPrefix+"*")
	if err != nil {
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}
	info, err := tmp.Stat()
	if err != nil {
		_ = tmp.Close()
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}

	if err := os.Link(tmp.Name(), lockPath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}
	return &stackLock{path: lockPath, content: content, info: info}, true, nil
}

// release removes the lock if it is still the one the step created (same file,
// same nonce): a lock that someone else removed (housekeeping's stale-lock
// cleanup) and that git or another step took again is never removed.
func (l *stackLock) release() error {
	current, err := os.Lstat(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release the reference lock: %w", err)
	}
	content, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release the reference lock: %w", err)
	}
	if !os.SameFile(current, l.info) || !bytes.Equal(content, l.content) {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("release the reference lock: %w", err)
	}
	return nil
}

// removeStaleStackLock removes the reference lock of the repository at path
// when a durable step of a process that died left it: git's writes would fail
// on it until housekeeping removes stale locks. A lock of git is never
// removed. Only the owner calls it, when it takes the ownership: the durable
// steps of the process that owned the storage before have ended with it.
func removeStaleStackLock(path string) error {
	lockPath := filepath.Join(path, "reftable", "tables.list.lock")
	content, err := os.ReadFile(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the reference lock: %w", err)
	}
	if !bytes.HasPrefix(content, []byte(stackLockTag)) {
		return nil
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove a stale reference lock: %w", err)
	}
	return nil
}

// removeStepTemporaries removes the temporary files that a durable step of a
// process that died left in the repository at path.
func removeStepTemporaries(path string) error {
	leftovers, err := filepath.Glob(filepath.Join(path, stepTempPrefix+"*"))
	if err != nil {
		return err
	}
	for _, leftover := range leftovers {
		if err := os.Remove(leftover); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove a temporary file of a durable step: %w", err)
		}
	}
	return nil
}

// syncStack makes durable the tables of the stack not made durable yet in this
// dirty period, then the list and the directory. synced keeps only the tables
// the stack still lists.
func (d *Durability) syncStack(path string, st *refState) error {
	reftable := filepath.Join(path, "reftable")
	names, err := readTablesList(filepath.Join(reftable, "tables.list"))
	if err != nil {
		return fmt.Errorf("read the table list: %w", err)
	}

	d.mu.Lock()
	listed := make(map[string]struct{}, len(names))
	var pending []string
	for _, name := range names {
		listed[name] = struct{}{}
		if _, ok := st.synced[name]; !ok {
			pending = append(pending, name)
		}
	}
	for name := range st.synced {
		if _, ok := listed[name]; !ok {
			delete(st.synced, name)
		}
	}
	d.mu.Unlock()

	for _, name := range pending {
		if err := d.syncPath(filepath.Join(reftable, name)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Compacted away after the list was read: the next list names
				// the table that replaced it.
				continue
			}
			return err
		}
		d.mu.Lock()
		st.synced[name] = struct{}{}
		d.mu.Unlock()
	}

	for _, p := range []string{filepath.Join(reftable, "tables.list"), reftable} {
		if err := d.syncPath(p); err != nil {
			return err
		}
	}
	return nil
}

// Forget drops the state and the marker of the repository at path, which the
// caller moved aside under the syncer's repository lock. A writer that still
// writes to the moved repository writes to a directory that is removed.
func (d *Durability) Forget(ctx context.Context, repo RepoRef, path string) error {
	if d == nil {
		return nil
	}

	markerPath, err := d.markerPath(ctx, repo, path)
	if err != nil {
		return err
	}

	d.mu.Lock()
	st := d.repos[path]
	d.mu.Unlock()
	if st != nil {
		st.file.Lock()
		defer st.file.Unlock()
	}

	d.mu.Lock()
	live := st != nil && d.repos[path] == st && st.nLive > 0
	d.mu.Unlock()
	if live {
		// A git process that writes without fsync is still running. It names the
		// repository by its path, so its later writes land in what is there now
		// (the hydrated repository, say): the repository stays dirty, with a plain
		// marker, until it exited and a durable step made the stack durable.
		content, err := json.Marshal(marker{RepoRef: repo, BootID: d.bootID})
		if err != nil {
			return fmt.Errorf("encode the dirty marker: %w", err)
		}
		if err := d.writeDurably(markerPath, content); err != nil {
			return fmt.Errorf("write the dirty marker: %w", err)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if !st.dirty {
			st.dirty = true
			d.dirty.Inc()
		}
		st.repo = repo
		st.marker = markerPath
		st.untrusted = nil
		st.recovered = false
		st.synced = map[string]struct{}{}
		return nil
	}

	if err := os.Remove(markerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the dirty marker: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if st != nil && d.repos[path] == st {
		if st.dirty {
			d.dirty.Dec()
		}
		delete(d.repos, path)
	}
	return nil
}

// Recovered is a dirty repository found by Recover.
type Recovered struct {
	Repo RepoRef
	Path string
	// Reboot reports that the marker is of another kernel boot: the repository
	// must be moved aside before anything reads it.
	Reboot bool
	// Untrusted reports that a durable step of the previous process found a
	// failed fsync: the repository must be moved aside too, and is never
	// settled.
	Untrusted bool
}

// Recover reads the dirty markers of the storages; only the owner calls it,
// when it takes the ownership. A marker of the running boot makes its
// repository dirty again, to be settled once no git process of the previous
// owner can still write it (gitActive, and the recovery grace), or untrusted
// again when the marker says so; what a durable step of the previous owner
// left (its reference lock, its temporary files) is removed. It returns every
// repository found; the caller moves aside the ones of another boot and the
// untrusted ones before serving. A marker that cannot be read is an error: its
// repository is unknown, and serving could read a torn stack.
func (d *Durability) Recover(ctx context.Context, storages []string) ([]Recovered, error) {
	if d == nil || !d.ownsLocks() {
		return nil, nil
	}

	var found []Recovered
	for _, storageName := range storages {
		dir, err := d.markerDir(ctx, storageName)
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the dirty markers of storage %q: %w", storageName, err)
		}

		for _, entry := range entries {
			markerPath := filepath.Join(dir, entry.Name())
			if entry.Name() == ownerLockName {
				continue
			}
			if strings.HasPrefix(entry.Name(), markerTempPrefix) {
				// A temporary file of a marker write that did not complete: the
				// write never returned, so no reference write relied on it.
				if err := os.Remove(markerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return nil, fmt.Errorf("remove an incomplete dirty marker: %w", err)
				}
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				return nil, fmt.Errorf("unexpected file %q in the dirty marker directory", markerPath)
			}

			content, err := os.ReadFile(markerPath)
			if err != nil {
				return nil, fmt.Errorf("read the dirty marker %q: %w", markerPath, err)
			}
			var m marker
			if err := json.Unmarshal(content, &m); err != nil || m.Storage != storageName || m.RelativePath == "" || m.BootID == "" {
				return nil, fmt.Errorf("corrupt dirty marker %q: the repository it names cannot be trusted and is unknown", markerPath)
			}

			path, err := d.locator.GetRepoPath(ctx, repoOf(m.RepoRef), storage.WithRepositoryVerificationSkipped())
			if err != nil {
				return nil, fmt.Errorf("dirty marker %q: %w", markerPath, err)
			}

			reboot := m.BootID != d.bootID
			if !reboot {
				if err := removeStaleStackLock(path); err != nil {
					return nil, err
				}
				if err := removeStepTemporaries(path); err != nil {
					return nil, err
				}
				d.mu.Lock()
				st := d.stateLocked(path)
				if !st.dirty {
					st.dirty = true
					d.dirty.Inc()
				}
				st.repo = m.RepoRef
				st.marker = markerPath
				st.recovered = true
				st.settleAfter = time.Now().Add(d.recoveryGrace)
				if m.Untrusted != "" {
					st.untrusted = errors.New(m.Untrusted)
				}
				d.mu.Unlock()
			}
			found = append(found, Recovered{Repo: m.RepoRef, Path: path, Reboot: reboot, Untrusted: m.Untrusted != ""})
		}
	}
	return found, nil
}

// CountUntrusted counts a repository moved aside for reason.
func (d *Durability) CountUntrusted(reason string) {
	if d == nil {
		return
	}
	d.untrusted.WithLabelValues(reason).Inc()
}

// ensureDir creates the directory dir when it does not exist, and makes its
// entry and the entry of a parent it created durable.
func (d *Durability) ensureDir(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	parent := filepath.Dir(dir)
	_, parentErr := os.Stat(parent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := d.syncPath(parent); err != nil {
		return err
	}
	if errors.Is(parentErr, fs.ErrNotExist) {
		return d.syncPath(filepath.Dir(parent))
	}
	return nil
}

// writeDurably writes content to path through a temporary file in the same
// directory, made durable before and after the rename.
func (d *Durability) writeDurably(path string, content []byte) (returnedErr error) {
	dir := filepath.Dir(path)
	if err := d.ensureDir(dir); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, markerTempPrefix+"*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if returnedErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := d.sync(tmp); err != nil {
		return writebackError{err: err}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return d.syncPath(dir)
}

// syncPath makes the file or directory at path durable. A failed open is
// returned as it is; a failed fsync is a writebackError.
func (d *Durability) syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := d.sync(f); err != nil {
		return writebackError{err: err}
	}
	return nil
}

// readTablesList returns the table names of a reftable stack.
func readTablesList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var names []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if name := strings.TrimSpace(scanner.Text()); name != "" {
			names = append(names, name)
		}
	}
	return names, scanner.Err()
}

// Describe implements prometheus.Collector.
func (d *Durability) Describe(descs chan<- *prometheus.Desc) {
	d.marked.Describe(descs)
	d.cleared.Describe(descs)
	d.untrusted.Describe(descs)
	d.stepErrors.Describe(descs)
	d.dirty.Describe(descs)
	d.inflight.Describe(descs)
	d.owning.Describe(descs)
}

// Collect implements prometheus.Collector.
func (d *Durability) Collect(metrics chan<- prometheus.Metric) {
	d.marked.Collect(metrics)
	d.cleared.Collect(metrics)
	d.untrusted.Collect(metrics)
	d.stepErrors.Collect(metrics)
	d.dirty.Collect(metrics)
	d.inflight.Collect(metrics)
	d.owning.Collect(metrics)
}
