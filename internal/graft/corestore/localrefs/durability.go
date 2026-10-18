package localrefs

import (
	"bufio"
	"bytes"
	"context"
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

// bootIDPath is where Linux exposes the identity of the running kernel boot.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

const (
	// DurableStepInterval is how often the durable step runs for every dirty
	// repository: it bounds how long a write failure the kernel reports goes
	// unnoticed.
	DurableStepInterval = time.Second
	// DurableStepIdle is how long a dirty repository must be without a reference
	// write before the durable step removes its marker.
	DurableStepIdle = time.Second
)

// CrashEnv names the test-only environment variable that makes the process
// exit (exit code 75) in the durable step, while it holds git's reference lock
// after making the stack durable and before it removes the marker: "durable_step".
// The crash tests with real binaries use it. It is read once by NewDurability.
const CrashEnv = "GRAFT_STORAGE_TEST_CRASH"

const (
	crashDurableStep = "durable_step"
	crashExitCode    = 75
)

// stackLockContent is what the durable step writes into the reference lock
// it takes. git never writes it (a lock of git holds table names), so a lock
// with this content found at startup was left by a durable step that died.
const stackLockContent = "graft durable step\n"

// ErrUntrusted is returned by Settle when making the references of a
// repository durable failed: the kernel may have dropped some of their writes,
// so the local repository must not be served before it is rebuilt.
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

// Durability defers the durability of the local reference writes of the
// storage core in authoritative mode (doc/graft/PLAN7.md, LOCAL REFERENCE
// WRITES RULE). The database commit of a transaction is its durable commit
// point and the local repository is a cache of it, so git writes the references
// without fsync and Durability keeps the cache safe to serve:
//
//   - before the first reference write that is not durable, the repository
//     gets a dirty marker, written durably, that holds the identity of the
//     kernel boot (MarkDirty);
//   - a durable step makes the references durable and removes the marker,
//     under git's reference lock and only when no writer is between its
//     Prepared and Committed phases (Settle);
//   - at startup, a marker of another boot means the kernel restarted while
//     the repository had writes that may never have reached the disk: its
//     reftable stack may be torn, which git reads as an empty repository, so
//     the repository is moved aside and hydrated again (Recover). A marker of
//     the running boot is a process restart: the kernel kept every write.
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

	mu sync.Mutex
	// repos holds the state of every repository written since the process
	// started (one entry per repository; Forget drops it).
	repos map[string]*refState

	marked    prometheus.Counter
	cleared   prometheus.Counter
	untrusted *prometheus.CounterVec
	dirty     prometheus.Gauge
	inflight  prometheus.Gauge
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
	// inflight counts, by key, the hook transactions between their Prepared
	// and Committed phases: git may still write their table and compact the
	// stack without fsync.
	inflight  map[Key]int
	nInflight int
	lastWrite time.Time
	// synced holds the tables made durable during the current dirty period.
	// Tables are immutable, so one fsync each is enough.
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
		locator: locator,
		idle:    idle,
		sync:    (*os.File).Sync,
		crashAt: os.Getenv(CrashEnv),
		repos:   map[string]*refState{},
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
		dirty: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "graft_storage_ref_dirty_repositories",
			Help: "Number of local repositories with reference writes that are not durable yet.",
		}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "graft_storage_ref_writes_inflight",
			Help: "Number of hook reference transactions between their Prepared and Committed phases. One that never reports Committed keeps its repository dirty.",
		}),
	}
}

// Enabled reports whether the reference writes of the storage core are made
// without fsync.
func (d *Durability) Enabled() bool { return d != nil }

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
		st = &refState{inflight: map[Key]int{}, synced: map[string]struct{}{}}
		d.repos[path] = st
	}
	return st
}

// MarkDirty records that the repository at path is about to get reference
// writes that are not durable. When it returns nil the repository has a durable
// dirty marker; the writer must not write before. The caller holds git's
// reference lock of the repository (the voter's Prepared phase) or the syncer's
// repository lock, under which Settle decides to clear the marker, and the
// marker file is only written or removed under the state's file lock, so a
// marker is never removed after a writer relied on it.
func (d *Durability) MarkDirty(ctx context.Context, repo RepoRef, path string) error {
	if d == nil {
		return nil
	}

	d.mu.Lock()
	st := d.stateLocked(path)
	st.lastWrite = time.Now()
	d.mu.Unlock()

	st.file.Lock()
	defer st.file.Unlock()

	d.mu.Lock()
	dirty := st.dirty
	d.mu.Unlock()
	if dirty {
		return nil
	}

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
	st.dirty = true
	st.repo = repo
	st.marker = markerPath
	d.marked.Inc()
	d.dirty.Inc()
	return nil
}

// Begin records that the hook transaction key is past its Prepared phase on
// the repository at path: git may write its table and compact the stack until
// it reports Committed (End). It is called under git's reference lock.
func (d *Durability) Begin(path string, key Key) {
	if d == nil {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.stateLocked(path)
	st.inflight[key]++
	st.nInflight++
	d.inflight.Inc()
}

// End records that the hook transaction key committed. A transaction aborted
// after its Prepared phase is not ended: two transactions with the same key
// cannot be told apart, and ending the wrong one would let Settle remove the
// marker while the other one still writes. Its repository stays dirty instead.
func (d *Durability) End(path string, key Key) {
	if d == nil {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.repos[path]
	if st == nil || st.inflight[key] == 0 {
		return
	}
	st.inflight[key]--
	if st.inflight[key] == 0 {
		delete(st.inflight, key)
	}
	st.nInflight--
	d.inflight.Dec()
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

// Settle is the durable step of the repository at path. It makes the tables
// that the stack lists, the list and the directory durable, which reports a
// write the kernel failed to do (an *UntrustedError: the caller moves the
// repository aside at once). When the repository has been idle for the idle
// period and no hook transaction is in flight, it then removes the marker:
// the decision is taken under git's reference lock, taken without waiting (a
// busy lock leaves the marker for the next step), and the marker file is
// removed after the lock is released, under the state's file lock that a
// writer's MarkDirty waits for. A repository that no longer exists (moved
// aside as a stale cache) loses its marker. The caller holds the syncer's
// repository lock, so no syncer write and no replacement of the repository
// runs meanwhile.
func (d *Durability) Settle(path string) error {
	if d == nil {
		return nil
	}

	d.mu.Lock()
	st := d.repos[path]
	if st == nil || !st.dirty {
		d.mu.Unlock()
		return nil
	}
	repo := st.repo
	idle := st.nInflight == 0 && time.Since(st.lastWrite) >= d.idle
	d.mu.Unlock()

	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		st.file.Lock()
		defer st.file.Unlock()
		d.clear(st)
		return d.removeMarker(st)
	}

	if err := d.syncStack(path, st); err != nil {
		return &UntrustedError{Repo: repo, Path: path, Err: err}
	}
	if !idle {
		return nil
	}

	unlock, ok, err := lockStack(path)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	cleared, err := d.settleLocked(path, st)
	if err != nil {
		_ = unlock()
		return &UntrustedError{Repo: repo, Path: path, Err: err}
	}
	if !cleared {
		return unlock()
	}

	// The state is clean from here on, and a writer that takes git's lock now
	// writes a new marker: it waits for the file lock, held until the old
	// marker is removed.
	defer st.file.Unlock()
	if err := unlock(); err != nil {
		return err
	}
	return d.removeMarker(st)
}

// settleLocked makes the tables written since the first pass durable and, when
// no hook transaction is in flight, marks the state clean with its file lock
// held (the caller releases it). The caller holds git's reference lock: no
// transaction can pass its Prepared phase meanwhile, and the transactions
// counted before have committed when the count is zero.
func (d *Durability) settleLocked(path string, st *refState) (bool, error) {
	d.mu.Lock()
	busy := st.nInflight > 0
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

// lockStack takes git's reference lock of the repository at path without
// waiting: ok is false when it is held. The lock is created with its content
// at once (a linked temporary file), so that a lock left by a process that
// died is recognized at startup (removeStaleStackLock).
func lockStack(path string) (unlock func() error, ok bool, err error) {
	lockPath := filepath.Join(path, "reftable", "tables.list.lock")

	tmp, err := os.CreateTemp(path, ".graft-durable-step-*")
	if err != nil {
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(stackLockContent); err != nil {
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
	return func() error {
		if err := os.Remove(lockPath); err != nil {
			return fmt.Errorf("release the reference lock: %w", err)
		}
		return nil
	}, true, nil
}

// removeStaleStackLock removes the reference lock of the repository at path
// when a durable step of a process that died left it: git's writes would fail
// on it until housekeeping removes stale locks. A lock of git is never
// removed.
func removeStaleStackLock(path string) error {
	lockPath := filepath.Join(path, "reftable", "tables.list.lock")
	content, err := os.ReadFile(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the reference lock: %w", err)
	}
	if !bytes.Equal(content, []byte(stackLockContent)) {
		return nil
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove a stale reference lock: %w", err)
	}
	return nil
}

// syncStack makes durable the tables of the stack not made durable yet in this
// dirty period, then the list and the directory.
func (d *Durability) syncStack(path string, st *refState) error {
	reftable := filepath.Join(path, "reftable")
	names, err := readTablesList(filepath.Join(reftable, "tables.list"))
	if err != nil {
		return fmt.Errorf("read the table list: %w", err)
	}

	d.mu.Lock()
	var pending []string
	for _, name := range names {
		if _, ok := st.synced[name]; !ok {
			pending = append(pending, name)
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

	if err := os.Remove(markerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the dirty marker: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if st != nil && d.repos[path] == st {
		if st.dirty {
			d.dirty.Dec()
		}
		d.inflight.Sub(float64(st.nInflight))
		delete(d.repos, path)
	}
	return nil
}

// Recovered is a dirty repository found at startup.
type Recovered struct {
	Repo RepoRef
	Path string
	// Reboot reports that the marker is of another kernel boot: the repository
	// must be moved aside before anything reads it.
	Reboot bool
}

// Recover reads the dirty markers of the storages. A marker of the running
// boot makes its repository dirty again, to be settled, and a reference lock
// that a durable step of the previous process left is removed. It returns
// every repository found; the caller moves aside the ones of another boot
// before serving. A marker that cannot be read is an error: its repository is
// unknown, and serving could read a torn stack.
func (d *Durability) Recover(ctx context.Context, storages []string) ([]Recovered, error) {
	if d == nil {
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
			if !strings.HasSuffix(entry.Name(), ".json") {
				// A temporary file of a marker write that did not complete: the
				// write never returned, so no reference write relied on it.
				if err := os.Remove(markerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return nil, fmt.Errorf("remove an incomplete dirty marker: %w", err)
				}
				continue
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
				d.mu.Lock()
				st := d.stateLocked(path)
				if !st.dirty {
					st.dirty = true
					st.repo = m.RepoRef
					st.marker = markerPath
					d.dirty.Inc()
				}
				d.mu.Unlock()
			}
			found = append(found, Recovered{Repo: m.RepoRef, Path: path, Reboot: reboot})
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

// writeDurably writes content to path through a temporary file in the same
// directory, made durable before and after the rename.
func (d *Durability) writeDurably(path string, content []byte) (returnedErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".marker-*.tmp")
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
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return d.syncPath(dir)
}

// syncPath makes the file or directory at path durable.
func (d *Durability) syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return d.sync(f)
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
	d.dirty.Describe(descs)
	d.inflight.Describe(descs)
}

// Collect implements prometheus.Collector.
func (d *Durability) Collect(metrics chan<- prometheus.Metric) {
	d.marked.Collect(metrics)
	d.cleared.Collect(metrics)
	d.untrusted.Collect(metrics)
	d.dirty.Collect(metrics)
	d.inflight.Collect(metrics)
}
