package mvcc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/feder-cr/open_scion/internal/safe"
	"github.com/feder-cr/open_scion/internal/structerr"
	"golang.org/x/sync/errgroup"
)

const (
	// latestManifestPointer is the string used in `prepare` to indicate
	// that the latest manifest pointer should be fetched instead of a
	// specific one. The value of the string does not matter, as long as it
	// does not conflict with a real manifest hash.
	latestManifestPointer = ""
)

// EnvVar is a helper struct to allow for better handling of the
// environment variables returned by the Cache.
type EnvVar struct {
	Name  string
	Value string
}

// String returns the environment  in the format "key=val"
func (e EnvVar) String() string {
	return fmt.Sprintf("%s=%s", e.Name, e.Value)
}

// Cache handles all operations between an RPC and a repository's MVCC
// cache. A Cache instance should be created once per RPC, and it's lifetime
// should be tied to the lifetime of the RPC.
// It allows to warm a cache before Git commands can be executed on the
// repository, and to commit the changes at the end of the RPC.
type Cache interface {
	// Prepare prepares the local MVCC cache so that Git operations can
	// be performed on the repository. Under the hood, it should call
	// object storage, fetch the manifest pointer, and make sure all
	// required artifacts are in the mvcc cache.
	// This is an async call, it should not wait for the cache to be ready
	// before returning.
	Prepare(ctx context.Context)

	// PrepareWithManifest is used when the RPC already contains a manifest hash
	// in the metadata. Example: request coming from Gitaly hooks.
	// Instead of using the latestmanifest pointer, it will prepare the
	// cache to use this manifest hash and use it for the duration
	// of the RPC.
	PrepareWithManifest(ctx context.Context, hash string)

	// WaitUntilReady is a blocking call, and should unblock when the mvcc cache
	// is ready. This is useful to start the process of updating the cache
	// ahead of time, in parallel with other things, so that once the handler
	// is ready to execute Git commands, the cache has already started warming up.
	WaitUntilReady(ctx context.Context) error

	// Commit commits the result of the RPC (only Mutator RPC).
	Commit(ctx context.Context) error

	// SetCommitConflictError registers the error Commit returns instead of
	// ErrManifestPointerConflict when CAS fails on the manifest pointer.
	SetCommitConflictError(err error)

	// AfterCommit registers fn to run once Commit has published the changes
	// of the RPC. It returns false, and fn never runs, when the cache is
	// read-only or the RPC has already started committing or been cleaned up.
	// fn does not run either when the RPC fails or Commit fails, as the
	// changes of the RPC are then thrown away. It may be called concurrently
	// with the RPC, for example from a hook request served while the RPC waits
	// for Git.
	AfterCommit(fn func()) bool

	// Environment returns the list of environment variable to use
	// when invoking Git.
	// Ex:
	// - `GIT_MVCC_MANIFEST`
	// - `GIT_MVCC_MANIFEST_PATH`
	Environment() []EnvVar

	// BaseManifestPointer returns the base manifest pointer of the
	// repository, the one resolved at the start of the RPC.
	// WaitUntilReady must be called prior to calling this because
	// it has to wait for the manifest to be fetched before it can
	// return the value.
	BaseManifestPointer() string

	// ReadOnly returns true if the RPC is a read only RPC or false
	// otherwise.
	ReadOnly() bool

	// WalkManifest visits every artifact of the manifest that this cache
	// was prepared against.
	WalkManifest(ctx context.Context, visitor ArtifactVisitor) error
}

// LocalCache is an implementation of Cache targeting the repository MVCC cache
// on the local filesystem.
//
// A cache belongs to one RPC and has no internal synchronization: Prepare, Commit and Cleanup
// must be called from a single goroutine, which is what the middleware does.
type LocalCache struct {
	// rs is the ObjectStorage used to communicate with persistent storage
	// to fetch the manifests and Git artifacts
	rs ObjectStorage

	// repositoryID is the repository ID for the repository related to the
	// current instance of the MVCCCache. While this might be a relative
	// path (i.e.: @hashed/ab/cd/abcd...), it is purposely  called repository
	// **ID** because it could theoretically be any kind of IDs. The concept
	// of relative path is foreign to the cache implementation.
	repositoryID string

	// This the runtime directory of Gitaly. This is where sockets and other Gitaly related files
	// are created during the lifetime of the Gitaly process. This is where we will also create the
	// manifests file for writes RPC.
	runtimeDir string

	// storagePath is the path where to find the repository on disk
	storagePath string

	// environments holds the environment variables that must be passed down
	// to the Git commands executing on this repository for the proper mvcc
	// manifest top be used.
	environments map[string]string

	// readOnly tells if the RPC related to this cache instance is a read-only
	// RPC or not.
	readOnly bool

	// prepareDone is used to synchronize between the prepare phase and the ready phase.
	// This channel gets closed one cache warming is done.
	prepareDone chan struct{}

	// readyCh is closed once the preparation phase is done. It allows to wait for the
	// prepare phase to finish, so that we can capture the error into prepareErr.
	readyDone chan struct{}

	// prepareErr is the error returned by `prepare()`. We cache this value so that
	// subsequent call to `WaitForReady` get the same value everytime. This insures
	// consistent behavior when called multiple time.
	prepareErr error

	// prepareOnce makes sure the cache is only warmed once.
	prepareOnce sync.Once

	// readyOnce insures that, if `prepare` returns an error, multiple calls to
	// WaitForReady returns that same error. Unless of course the context is canceled.
	// In that case it returns the context error.
	readyOnce sync.Once

	// base is the manifest pointer resolved at the start of a mutator RPC. It is
	// the base against which the commit-time compare-and-swap is performed, and it
	// carries the generation (when the backend tracks one) so Commit can advance
	// without re-reading the head.
	base ManifestPointer

	// baseManifest is the parsed manifest referenced by base.Hash. It is used to
	// compute which artifacts a mutator RPC newly produced.
	baseManifest *Manifest

	// committed guards Commit so the canonical manifest pointer is advanced at most
	// once per RPC.
	committed bool

	// commitConflictErr is the error registered via SetCommitConflictError. Commit
	// returns it instead of ErrManifestPointerConflict when CAS fails on the
	// manifest pointer.
	commitConflictErr error

	// afterCommitMu guards afterCommit, afterCommitClosed and commitHandle. Unlike the rest
	// of the cache, they are used from other goroutines than the RPC's: AfterCommit and
	// CacheByCommitHandle are called by the hook requests that Git sends while the RPC waits
	// for it.
	afterCommitMu sync.Mutex
	// afterCommit are the functions registered with AfterCommit, in registration order.
	afterCommit []func()
	// afterCommitClosed is set once Commit or Cleanup took the registered functions, after
	// which AfterCommit refuses new ones.
	afterCommitClosed bool
	// commitHandle is the handle that CommitHandle returned for this cache, if any.
	commitHandle string

	// unlockRepository releases the repository lock. It is set for mutators only, and only
	// once the cache has been prepared. Preparing a cache without cleaning it up leaves the
	// repository locked for the lifetime of the process.
	unlockRepository func()

	// metrics records prepare latency and commit outcomes. It is nil for caches
	// built outside the CacheFactory (e.g. in tests), in which case recording is
	// skipped.
	metrics *cacheMetrics
}

// Interface compliance check.
var _ Cache = &LocalCache{}

// NewLocalCache returns a new MVCCCache pointer.
func NewLocalCache(rs ObjectStorage, runtimeDir string, storagePath string, repositoryID string, readOnly bool) *LocalCache {
	return &LocalCache{
		rs:           rs,
		repositoryID: repositoryID,
		runtimeDir:   runtimeDir,
		storagePath:  storagePath,
		environments: make(map[string]string),
		readOnly:     readOnly,
		readyDone:    make(chan struct{}),
		prepareDone:  make(chan struct{}),
	}
}

// Prepare calls PrepareWithManifest under the hood with an empty manifest hash, which semantically
// means to warm the cache using the current manifest pointer.
func (m *LocalCache) Prepare(ctx context.Context) {
	m.PrepareWithManifest(ctx, latestManifestPointer)
}

// PrepareWithManifest kicks off cache warming in the background and returns immediately. The result
// of the warm-up (nil on success, or the first error encountered) is delivered to the ready channel,
// which  WaitUntilReady consumes.
// Repeated calls are no-ops: the work runs at most once per cache instance.
// The hash parameter must be the manifest hash to use for cache warming.
//
// For a mutator the warm-up first locks the repository against garbage collection, and it stays
// locked until Cleanup is called. That covers the window in which Git has written an artifact but
// Commit has not published the manifest referencing it. Callers must therefore always pair Prepare
// with Cleanup, or the repository would never be garbage collected again.
//
// Failing to lock the repository is reported like any other warm-up failure, so that this call
// stays asynchronous even when garbage collection is holding the repository.
func (m *LocalCache) PrepareWithManifest(ctx context.Context, hash string) {
	m.prepareOnce.Do(func() {
		go func() {
			defer close(m.prepareDone)

			if !m.readOnly {
				unlock, err := LockRepositoryForWrite(ctx, m.repositoryPath())
				if err != nil {
					m.prepareErr = err
					return
				}

				m.unlockRepository = unlock
			}

			m.prepareErr = m.prepare(ctx, hash)
		}()
	})
}

// WaitUntilReady blocks until one of those two conditions occur:
// 1. The context is canceled
// 2. The ready phase is done.
//
// If the ready phase is done, the same error returned in the prepare
// phase is returned every time. This makes it safe to call this function
// multiple time. Unless the context gets canceled, the same error as returned
// by the prepare phase will be returned every single time.
func (m *LocalCache) WaitUntilReady(ctx context.Context) error {
	m.readyOnce.Do(func() {
		go func() {
			<-m.prepareDone
			close(m.readyDone)
		}()
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.readyDone:
		return m.prepareErr
	}
}

// Commit publishes any MVCC artifacts that have been produced by the LocalCache, and
// updates the repository to point to the latest manifest.
func (m *LocalCache) Commit(ctx context.Context) (returnedErr error) {
	if m.readOnly {
		return nil
	}

	// Count the commit outcome. Read-only RPCs return above and are not counted.
	// The conflict is flagged at the compare-and-swap below rather than derived
	// from returnedErr, because a caller-registered conflict error (see
	// SetCommitConflictError) no longer matches ErrManifestPointerConflict.
	var conflict bool
	defer func() { m.recordCommit(returnedErr, conflict) }()

	// The handler has returned, so nothing registers functions anymore. They run only
	// once the changes are published, and are dropped on every error path below.
	afterCommit := m.takeAfterCommit()

	// Wait for warming to finish before reading environments, which the background
	// prepare goroutine writes. A background context is used so the wait ignores request
	// cancellation.
	if err := m.WaitUntilReady(context.Background()); err != nil {
		return err
	}

	if m.ManifestPath() == "" {
		return fmt.Errorf("empty GIT_MVCC_MANIFEST_PATH")
	}

	// Assert the at-most-once property: advancing the canonical pointer more than
	// once for a single RPC would result in a torn write.
	if m.committed {
		return fmt.Errorf("mvcc: commit called more than once")
	}
	m.committed = true

	newHashBytes, err := os.ReadFile(m.ManifestPath())
	if err != nil {
		return fmt.Errorf("read from manifest path: %w", err)
	}
	newHash := strings.TrimSuffix(string(newHashBytes), "\n")
	if newHash == m.base.Hash {
		runAfterCommit(afterCommit)
		return nil // no changes, nothing to publish
	}

	// Upload the new artifacts before advancing the pointer so the pointer never
	// references an artifact that is not yet durable.
	if err := m.putNewArtifacts(ctx, newHash); err != nil {
		return fmt.Errorf("put new artifacts: %w", err)
	}

	// The functions registered with AfterCommit run in the order in which the RPCs of this
	// process advance the pointer. An RPC that builds on the changes of another one can only
	// commit after it, but without this lock its functions could still run first.
	if len(afterCommit) > 0 {
		lock, dropReference := repositoryLocks.acquire(m.repositoryPath())
		lock.commitOrder.Lock()
		defer func() {
			lock.commitOrder.Unlock()
			dropReference()
		}()
	}

	// Atomically advance the canonical manifest pointer.
	if err := m.rs.Commit(ctx, m.repositoryID, m.base, newHash); err != nil {
		if errors.Is(err, ErrManifestPointerConflict) {
			conflict = true
			if m.commitConflictErr != nil {
				// Carry through the original error message.
				return structerr.New("%w: %s", m.commitConflictErr, err.Error())
			}
		}
		return err
	}

	runAfterCommit(afterCommit)

	return nil
}

// AfterCommit registers fn to run once Commit has published the changes of the RPC, see
// Cache.AfterCommit.
func (m *LocalCache) AfterCommit(fn func()) bool {
	if m.readOnly {
		return false
	}

	m.afterCommitMu.Lock()
	defer m.afterCommitMu.Unlock()

	if m.afterCommitClosed {
		return false
	}

	m.afterCommit = append(m.afterCommit, fn)
	return true
}

// takeAfterCommit returns the functions registered with AfterCommit and refuses new ones from
// now on. It also unregisters the commit handle of the cache, as nothing can be registered
// through it anymore.
func (m *LocalCache) takeAfterCommit() []func() {
	m.afterCommitMu.Lock()
	defer m.afterCommitMu.Unlock()

	m.afterCommitClosed = true
	if m.commitHandle != "" {
		commitHandles.Delete(m.commitHandle)
	}

	afterCommit := m.afterCommit
	m.afterCommit = nil
	return afterCommit
}

// runAfterCommit runs the given functions in order.
func runAfterCommit(afterCommit []func()) {
	for _, fn := range afterCommit {
		fn()
	}
}

// commitHandles maps the handles returned by CommitHandle to their caches.
var commitHandles sync.Map

// CommitHandle returns a handle under which CacheByCommitHandle finds the mutator cache c until
// it commits or is cleaned up. The RPC passes it to the processes it spawns, such as the
// reference-transaction hooks of Git, so that the requests those processes send back to the
// server can register functions with AfterCommit on the RPC's cache. It returns an empty string
// for read-only caches and for caches that have no handles.
func CommitHandle(c Cache) string {
	cache, ok := c.(*LocalCache)
	if !ok || cache.readOnly {
		return ""
	}

	cache.afterCommitMu.Lock()
	defer cache.afterCommitMu.Unlock()

	if cache.afterCommitClosed {
		return ""
	}

	if cache.commitHandle == "" {
		cache.commitHandle = rand.Text()
		commitHandles.Store(cache.commitHandle, cache)
	}

	return cache.commitHandle
}

// CacheByCommitHandle returns the cache that CommitHandle returned the handle for, or nil when
// that cache has committed or been cleaned up, or never existed in this process.
func CacheByCommitHandle(handle string) Cache {
	if handle == "" {
		return nil
	}

	cache, ok := commitHandles.Load(handle)
	if !ok {
		return nil
	}

	return cache.(*LocalCache)
}

// SetCommitConflictError registers the error Commit returns instead of
// ErrManifestPointerConflict when its compare-and-swap fails.
func (m *LocalCache) SetCommitConflictError(err error) {
	m.commitConflictErr = err
}

// Cleanup removes temporary resources created by the LocalCache and unlocks the repository. It
// must be called once the RPC no longer touches the repository, which for a mutator means after
// Commit has published the new manifest.
func (m *LocalCache) Cleanup() {
	// Claim the warm-up if it never started, so that waiting for it below cannot block on a
	// channel that nothing is going to close. Preparing a cache that has been cleaned up is a
	// bug in the caller, so making that a no-op costs nothing.
	m.prepareOnce.Do(func() { close(m.prepareDone) })

	// Functions that a Commit did not take belong to an RPC whose changes are thrown away.
	_ = m.takeAfterCommit()

	// Wait for warming to finish before reading environments, which the background
	// prepare goroutine writes. Otherwise the deferred Cleanup in the mvcc interceptor
	// races with prepare when the handler returns before warming completes. A background
	// context is used so the wait ignores request cancellation; the goroutine may still
	// be writing environments even once the RPC is done.
	_ = m.WaitUntilReady(context.Background())

	if path := m.ManifestPath(); path != "" {
		_ = os.Remove(path)
	}

	if m.unlockRepository != nil {
		m.unlockRepository()
	}
}

// repositoryPath returns the absolute path of the repository this cache operates on. It is the
// identity the repository lock is taken under, so it must match what garbage collection uses.
func (m *LocalCache) repositoryPath() string {
	return filepath.Join(m.storagePath, m.repositoryID)
}

// Environment returns the list of environment variables that must be passed to
// each Git commands executed on the repository when using this MVCC cache.
func (m *LocalCache) Environment() []EnvVar {
	var result []EnvVar
	for k, v := range m.environments {
		result = append(result, EnvVar{Name: k, Value: v})
	}
	return result
}

// BaseManifestPointer returns the manifest pointer resolved at
// the start of the RPC.
func (m *LocalCache) BaseManifestPointer() string {
	return m.base.Hash
}

// ReadOnly returns true of the RPC is an accessor RPC.
// False otherwise.
func (m *LocalCache) ReadOnly() bool {
	return m.readOnly
}

// WalkManifest visits every artifact of the base manifest that m was prepared
// against. The resulting Artifact.Path values are absolute filepaths on disk.
func (m *LocalCache) WalkManifest(ctx context.Context, visitor ArtifactVisitor) error {
	if err := m.WaitUntilReady(ctx); err != nil {
		return fmt.Errorf("wait until ready: %w", err)
	}

	return m.baseManifest.walk(ctx, filepath.Join(m.storagePath, m.repositoryID, mvccCacheDir), visitor)
}

// ManifestHash is a helper function to retrieve the manifest hash
// used for a read-only RPC. It returns an empty string when not set
// or when the RPC is a mutator one. This function is not part of the
// Cache interface, and is mostly used for testing.
func (m *LocalCache) ManifestHash() string {
	return m.environments["GIT_MVCC_MANIFEST"]
}

// ManifestPath is a helper function to retrieve the manifest path
// used for a mutator RPC. It returns an empty string when not set
// or when the RPC is an accessor one. This function is not part of the
// Cache interface, and is mostly used for testing.
func (m *LocalCache) ManifestPath() string {
	return m.environments["GIT_MVCC_MANIFEST_PATH"]
}

// prepare resolves the manifest pointer, then downloads every reftable it
// references from durable storage into the local cache directory.
func (m *LocalCache) prepare(ctx context.Context, hash string) (err error) {
	start := time.Now()
	defer func() { m.recordPrepare(start, err) }()

	// base carries the generation when the pointer was resolved from durable
	// storage; a pin (explicit hash) leaves the generation unknown so Commit
	// falls back to reading the head.
	var base ManifestPointer

	// An empty hash means to fetch the latest pointer.
	if hash == latestManifestPointer {
		// Read the manifest pointer from the RemoteStorage
		base, err = m.rs.ReadManifestPointer(ctx, m.repositoryID)
		if err != nil {
			return fmt.Errorf("read manifest pointer: %w", err)
		}
		hash = base.Hash
	} else {
		base = ManifestPointer{Hash: hash}
	}

	manifest, err := m.prepareManifestFile(ctx, hash)
	if err != nil {
		return fmt.Errorf("prepare manifest file: %w", err)
	}

	m.base = base
	m.baseManifest = manifest

	// Warm the cache. We only care about reftables for now. Objects will
	// be taken care of at a later stage.
	err = m.prepareArtifacts(ctx, manifest.RefPaths())
	if err != nil {
		return fmt.Errorf("prepare artifacts: %w", err)
	}

	// Prepare environment variables
	if m.readOnly {
		m.environments["GIT_MVCC_MANIFEST"] = hash
	} else {
		// For mutator RPCs, we create a temp manifest pointer file that points to the manifest file
		tmpManifestFileDir := filepath.Join(m.runtimeDir, mvccCacheDir, m.repositoryID)
		err = os.MkdirAll(tmpManifestFileDir, 0o755)
		if err != nil {
			return fmt.Errorf("make manifest temp dir: %w", err)
		}

		tmpManifestFile, err := os.CreateTemp(tmpManifestFileDir, "manifest-*")
		if err != nil {
			return fmt.Errorf("create temp manifest file: %w", err)
		}
		defer func() { _ = tmpManifestFile.Close() }()
		if _, err := tmpManifestFile.WriteString(hash); err != nil {
			return fmt.Errorf("write temp manifest file: %w", err)
		}
		m.environments["GIT_MVCC_MANIFEST_PATH"] = tmpManifestFile.Name()
	}

	return nil
}

// prepareManifestFile will read the manifest file defined by `hash` and return its parsed form.
// If the manifest file already exist on the local filesystem, the file is read from the filesystem.
// Else, the manifest file is fetched from ObjectStorage and then created on the local filesystem for future use.
func (m *LocalCache) prepareManifestFile(ctx context.Context, hash string) (manifest *Manifest, err error) {
	// Construct the manifest path
	manifestFilePath := filepath.Join(m.storagePath, m.repositoryID, mvccCacheDir, "manifests", hash)

	// Verify if it already exists on the local filesystem or not
	existLocally, err := fileExist(manifestFilePath)
	if err != nil {
		return nil, fmt.Errorf("check if file exists: %w", err)
	}

	if !existLocally {
		// If it does not exist, read content from ObjectStorage
		manifestReader, err := m.rs.ReadManifest(ctx, m.repositoryID, hash)
		if err != nil {
			return nil, fmt.Errorf("read manifest file: %w", err)
		}

		defer func() {
			closeErr := manifestReader.Close()
			err = errors.Join(err, closeErr)
		}()

		// The manifest is small and is parsed in memory anyway. It is verified against its
		// name before it is written to the local cache, so that a corrupt or substituted
		// object in the storage is never cached under the name it was requested with.
		manifestBody, err := io.ReadAll(manifestReader)
		if err != nil {
			return nil, fmt.Errorf("read manifest body: %w", err)
		}
		if err := verifyManifestName(manifestBody, hash); err != nil {
			return nil, fmt.Errorf("verify manifest: %w", err)
		}

		// Create the manifest file on the local file system atomically. This is a protection against
		// potential concurrent requests trying to create the same manifest file.
		if err = createFileAtomic(ctx, manifestFilePath, bytes.NewReader(manifestBody)); err != nil {
			return nil, fmt.Errorf("write manifest: %w", err)
		}
	}

	// Now that it exist on disk, let's open the file.
	manifestFile, err := os.Open(manifestFilePath)
	if err != nil {
		return nil, fmt.Errorf("opening manifest %s: %w", manifestFilePath, err)
	}

	// Read the content in memory. These files should not be that large.
	// The Manifest parsing logic needs the whole file at once to parse it.
	manifestContent := bytes.Buffer{}
	_, err = manifestContent.ReadFrom(manifestFile)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}

	// Return the parsed manifest
	return ParseManifest(manifestContent.Bytes())
}

func (m *LocalCache) prepareArtifacts(ctx context.Context, artifacts []string) error {
	const numWorkers = 5

	// errgroup gives us a derived context that is canceled as soon as any worker
	// returns an error, which unblocks the feeder below and the other workers.
	g, ctx := errgroup.WithContext(ctx)

	// Feed each artifact into a channel consumed by all workers below. The
	// feeder stops early if the context is canceled (error or RPC cancellation).
	feedCh := make(chan string)
	g.Go(func() error {
		defer close(feedCh)
		for _, artifact := range artifacts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case feedCh <- artifact:
			}
		}
		return nil
	})

	for i := 0; i < numWorkers; i++ {
		g.Go(func() error {
			for artifact := range feedCh {
				if err := ctx.Err(); err != nil {
					return err
				}

				artifactLocalPath := filepath.Join(m.storagePath, m.repositoryID, mvccCacheDir, artifact)
				exist, err := fileExist(artifactLocalPath)
				if err != nil {
					return fmt.Errorf("check if file %s exist: %w", artifactLocalPath, err)
				}

				// If the file already exists, there is nothing else to do.
				if exist {
					continue
				}

				readArtifactOp := func(ctx context.Context, r io.ReadCloser) error {
					return createFileAtomic(ctx, artifactLocalPath, r)
				}

				// readArtifactOp will be executed only once across concurrent requests
				// for the same artifact. The request that triggers the fetch is called
				// the leader. For the followers (other concurrent request) they will
				// block on `ReadArtifact` until either the fetch completes or the request
				// times out.
				err = m.rs.ReadArtifact(ctx, m.repositoryID, artifact, readArtifactOp)
				if err != nil {
					return fmt.Errorf("read artifact %q: %w", artifact, err)
				}
			}
			return nil
		})
	}

	return g.Wait()
}

func (m *LocalCache) putNewArtifacts(ctx context.Context, newHash string) error {
	manifestFile, err := os.Open(filepath.Join(m.storagePath, m.repositoryID, mvccCacheDir, manifestDirectory, newHash))
	if err != nil {
		return fmt.Errorf("opening manifest: %w", err)
	}
	defer func() {
		_ = manifestFile.Close()
	}()
	manifestContent := bytes.Buffer{}
	_, err = manifestContent.ReadFrom(manifestFile)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	newManifest, err := ParseManifest(manifestContent.Bytes())
	if err != nil {
		return fmt.Errorf("parse new manifest: %w", err)
	}
	baseSet := make(map[string]struct{}, len(m.baseManifest.Paths))
	for _, p := range m.baseManifest.Paths {
		baseSet[p] = struct{}{}
	}
	newArtifacts := make([]string, 0, len(newManifest.Paths))
	manifestKey := filepath.Join(manifestDirectory, newHash)
	newArtifacts = append(newArtifacts, manifestKey)
	for _, p := range newManifest.Paths {
		if _, ok := baseSet[p]; !ok {
			newArtifacts = append(newArtifacts, p)
		}
	}
	for _, p := range newArtifacts {
		localPath := filepath.Join(m.storagePath, m.repositoryID, mvccCacheDir, p)
		f, err := os.Open(localPath)
		if err != nil {
			return fmt.Errorf("read artifact before uploading: %w", err)
		}
		if err := m.rs.PutArtifact(ctx, m.repositoryID, p, f); err != nil {
			_ = f.Close()
			return fmt.Errorf("upload artifact %s: %w", p, err)
		}
		_ = f.Close()
	}

	return nil
}

// verifyManifestName checks that the manifest body hashes to the name it was requested
// with. A manifest is content-addressed: its name is the hex SHA-256 of its payload, which
// is the body without the 32-byte trailer. The trailer itself is checked by ParseManifest.
func verifyManifestName(body []byte, name string) error {
	if len(body) < trailerSize {
		return fmt.Errorf("manifest %s is too short (%d bytes)", name, len(body))
	}

	payloadEnd := len(body) - trailerSize
	sum := sha256.Sum256(body[:payloadEnd])
	if got := hex.EncodeToString(sum[:]); got != name {
		return fmt.Errorf("manifest content hashes to %s, expected %s", got, name)
	}

	return nil
}

func fileExist(path string) (bool, error) {
	_, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat manifest: %w", err)
	}
	return true, nil
}

func createFileAtomic(ctx context.Context, path string, content io.Reader) error {
	w, err := safe.NewFileWriter(path)
	if err != nil {
		return err
	}

	defer func() { _ = w.Close() }()

	if _, err := io.Copy(w, content); err != nil { // w implements io.Writer
		return err
	}

	// Atomically rename temp file -> absPath.
	if err := w.Commit(ctx); err != nil {
		return err
	}
	return nil
}
