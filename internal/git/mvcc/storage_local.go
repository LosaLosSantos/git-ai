package mvcc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/feder-cr/open_scion/internal/gitaly/storage/mode"
	"github.com/feder-cr/open_scion/internal/safe"
)

const (
	manifestPointer   = "manifest"
	manifestDirectory = "manifests"
	mvccCacheDir      = "mvcc"
)

type artifactKey struct {
	repositoryID string
	key          string
}

// LocalStorage is an implementation of ObjectStorage. It is a very simple
// implementation in the sense that it executes solely in the context of an
// on-disk repository. It treats the current on-disk repository as the source
// of truth. This is useful as a first step towards MVCC since it allows to
// migrate repositories to MVCC without relying on an object store to store
// references and objects.
type LocalStorage struct {
	// storageRoot is the location on disk that contains all repositories.
	// When fetching artifacts, the repository on "remote storage"
	// is located by joining `storageRoot` with `repositoryId`.
	storageRoot string

	runnerCache *KeyedRunnerCache[artifactKey]

	// beforePointerLock, when set, runs in Commit after the pointer writer was
	// opened and before it is locked. Tests use it to commit concurrently in
	// that window.
	beforePointerLock func()
}

// Interface conformance check
var _ ObjectStorage = &LocalStorage{}

// NewLocalStorage creates a new instance of LocalStorage
func NewLocalStorage(storageRoot string) *LocalStorage {
	return &LocalStorage{
		storageRoot: storageRoot,
		runnerCache: NewKeyedRunnerCache[artifactKey](),
	}
}

// ReadManifestPointer reads the manifest pointer for the repository ID.
// In this LocalStorage implementation, it reads at ./.git/mvcc/manifest
// It also trims the trailing newline character, if any.
// LocalStorage does not track generations, so the returned ManifestPointer
// carries only the hash (HasGeneration is false); Commit uses the hash for its
// compare-and-swap.
func (l *LocalStorage) ReadManifestPointer(_ context.Context, repositoryID string) (ManifestPointer, error) {
	pointerPath := filepath.Join(l.storageRoot, repositoryID, mvccCacheDir, manifestPointer)
	raw, err := os.ReadFile(pointerPath)
	if err != nil {
		return ManifestPointer{}, err
	}
	return ManifestPointer{Hash: strings.TrimSuffix(string(raw), "\n")}, nil
}

// ReadManifest reads the manifest defined by hash for the given repository ID.
func (l *LocalStorage) ReadManifest(_ context.Context, repositoryID string, hash string) (io.ReadCloser, error) {
	manifestPath := filepath.Join(l.storageRoot, repositoryID, mvccCacheDir, manifestDirectory, hash)
	return os.Open(manifestPath)
}

// ReadArtifact ensures the artifact identified by key is fetched for the given repository.
//
// Concurrent readers of the same repositoryID/key are coalesced onto a single fetch that
// runs in the background under a detached context, so it survives cancellation of the
// caller that initiated it. `consume` is invoked at most once with an open reader for
// the artifact's source; it must fully read and persist it. All callers block until the
// fetch finishes and observe its error, unless their own ctx is canceled first, in which
// case the fetch keeps running for the remaining callers.
//
// This is useful for large artifacts where the request's context might time out before the
// artifact can be downloaded. This ensures the download continues in the background so that
// eventually subsequent callers will have access to the artifact.
func (l *LocalStorage) ReadArtifact(ctx context.Context, repositoryID, key string, consume func(ctx context.Context, r io.ReadCloser) error) error {
	do := func(ctx context.Context) error {
		path := filepath.Join(l.storageRoot, repositoryID, mvccCacheDir, key)
		r, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		return consume(ctx, r)
	}
	return l.runnerCache.Run(ctx, artifactKey{repositoryID: repositoryID, key: key}, do)
}

// PutArtifact writes `content` into `key`. No-op for local storage system since Git already write the new
// artifacts to local MVCC cache dir.
func (l *LocalStorage) PutArtifact(_ context.Context, repositoryID, key string, content io.ReadCloser) error {
	return nil
}

// Commit atomically advances the manifest pointer from baseHash to newHash with a
// compare-and-swap. As the local filesystem has no native conditional write, a lock
// file serializes concurrent committers so the read-compare-write is atomic. If the
// pointer no longer matches baseHash (a concurrent writer won), it returns
// ErrManifestPointerConflict without modifying the pointer.
func (l *LocalStorage) Commit(ctx context.Context, repositoryID string, base ManifestPointer, newHash string) error {
	pointerPath := filepath.Join(l.storageRoot, repositoryID, mvccCacheDir, manifestPointer)

	lockedFile, err := safe.NewLockingFileWriter(pointerPath, safe.LockingFileWriterConfig{
		FileWriterConfig: safe.FileWriterConfig{
			FileMode: mode.File,
		},
	})
	if err != nil {
		return fmt.Errorf("creating file writer: %w", err)
	}
	defer func() {
		_ = lockedFile.Close()
	}()

	if _, err := io.Copy(lockedFile, strings.NewReader(newHash)); err != nil {
		return fmt.Errorf("copy to writer: %w", err)
	}
	if l.beforePointerLock != nil {
		l.beforePointerLock()
	}
	if err := lockedFile.Lock(); err != nil {
		if errors.Is(err, safe.ErrFileAlreadyLocked) {
			return fmt.Errorf("manifest pointer locked: %w", ErrManifestPointerConflict)
		}
		// Lock also fails when the pointer was replaced between creating the writer
		// and taking the lock ("file concurrently modified"). A concurrent committer
		// won the race in that case, so it is a conflict like any other.
		if current, readErr := l.ReadManifestPointer(ctx, repositoryID); readErr == nil && current.Hash != base.Hash {
			return fmt.Errorf("lock manifest pointer: %w, base %q current %q: %w",
				ErrManifestPointerConflict, base.Hash, current.Hash, err)
		}
		return fmt.Errorf("lock manifest pointer: %w", err)
	}

	current, err := l.ReadManifestPointer(ctx, repositoryID)
	if err != nil {
		return fmt.Errorf("read current pointer: %w", err)
	}
	// LocalStorage has no generations, so the compare-and-swap is on the hash.
	if current.Hash != base.Hash {
		return fmt.Errorf("base manifest pointer changed: %w, base %q current %q",
			ErrManifestPointerConflict, base.Hash, current.Hash)
	}
	if err := lockedFile.Commit(ctx); err != nil {
		return fmt.Errorf("commit writer: %w", err)
	}

	return nil
}

// TearDown drains the keyedRunner cache.
func (l *LocalStorage) TearDown(_ context.Context) error {
	l.runnerCache.Drain()
	return nil
}
