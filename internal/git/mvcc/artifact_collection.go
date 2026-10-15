package mvcc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/feder-cr/open_scion/internal/log"
)

// errNoManifests is returned when the manifest directory holds nothing to judge reachability by.
var errNoManifests = errors.New("no manifests")

// ArtifactCollectionConfig configures an artifact collection run.
type ArtifactCollectionConfig struct {
	// ArtifactMinAge is how old an unreferenced entry must be before it may be removed. It is
	// raised to minEntryAge if it is shorter than that.
	ArtifactMinAge time.Duration
	// MaxLockDuration is the budget for what a run does while holding the repository lock.
	// A run stops taking on new work once the budget is spent, but nothing interrupts a step
	// that has already begun, so a slow unlink can overrun it. Entries left over when it runs
	// out are collected by the next run.
	MaxLockDuration time.Duration
	// MaxAcquireWait is how long a run may wait for the repository lock before giving up.
	MaxAcquireWait time.Duration
}

// ArtifactCollectionResult reports what an artifact collection run did.
type ArtifactCollectionResult struct {
	// Removed is the number of entries removed.
	Removed int
	// Skipped is set when the run gave up because the repository stayed locked for writing.
	Skipped bool
	// Cancelled is set when the run gave up because its context was cancelled.
	Cancelled bool
	// Evicted is set when the run stopped early to hand the repository to a write.
	Evicted bool
	// AcquireDuration is how long the run spent waiting to lock the repository.
	AcquireDuration time.Duration
	// LockDuration is how long the run held the lock for.
	LockDuration time.Duration
}

// artifactEntry is a file below the MVCC directory that a run may remove, keyed the way manifests
// reference it.
type artifactEntry struct {
	relativePath string
	modTime      time.Time
}

// CollectArtifacts removes the entries below the MVCC directory that no manifest references and
// that are old enough to no longer be a fetch in progress. Reachability is the union of every
// manifest on disk. The repository lock is taken only to delete.
func CollectArtifacts(ctx context.Context, repoPath string, cfg ArtifactCollectionConfig) (result ArtifactCollectionResult, _ error) {
	if cfg.MaxLockDuration <= 0 {
		return result, fmt.Errorf("no time budget to delete under")
	}

	mvccPath := filepath.Join(repoPath, mvccCacheDir)

	allArtifacts, err := scanArtifacts(mvccPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The repository holds no MVCC data yet.
			return result, nil
		}
		return result, fmt.Errorf("scanning MVCC directory: %w", err)
	}

	reachable := newReachability()
	if err := reachable.refresh(ctx, mvccPath); err != nil {
		if ctx.Err() != nil {
			result.Cancelled = true
			return result, nil
		}
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNoManifests) {
			// With no manifest to go by every entry would look unreferenced, which is
			// not a reason to delete anything.
			return result, nil
		}
		return result, err
	}

	minAge := max(cfg.ArtifactMinAge, minEntryAge)

	unreferenced := deletable(allArtifacts, minAge, reachable)
	if len(unreferenced) == 0 {
		return result, nil
	}

	lock, waited, skipped, cancelled := acquire(ctx, repoPath, cfg.MaxAcquireWait)
	result.AcquireDuration, result.Skipped, result.Cancelled = waited, skipped, cancelled
	if lock == nil {
		return result, nil
	}
	defer lock.Unlock()

	// The lock is held from here on, so the refresh below is on the same budget as the
	// deletions.
	deleteCtx, cancel := context.WithTimeout(ctx, cfg.MaxLockDuration)
	defer cancel()

	// Registered after the unlock, so it runs before it and still sees the lock as held.
	defer func() {
		result.Evicted = lock.Evicted()
		result.Cancelled = ctx.Err() != nil
		result.LockDuration = lock.HeldFor()
	}()

	// A write may have published a manifest while we waited for the lock. Giving up here
	// removes nothing, since what is reachable would only be partly known.
	if err := reachable.refresh(deleteCtx, mvccPath); err != nil {
		if deleteCtx.Err() != nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNoManifests) {
			return result, nil
		}
		return result, err
	}

	result.Removed = removeStale(deleteCtx, mvccPath, unreferenced, minAge, lock, reachable)

	return result, nil
}

// scanArtifacts lists the files below the MVCC directory that a run may remove, keyed the way
// manifests reference them. The manifest directory is left out, as manifest pruning owns it, and so
// is the pointer. Only files are listed, so a directory left empty by a run stays behind.
func scanArtifacts(mvccPath string) ([]artifactEntry, error) {
	var entries []artifactEntry

	// The paths os.DirFS yields are slash-separated, which is how manifests reference their
	// artifacts.
	err := fs.WalkDir(os.DirFS(mvccPath), ".", func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			// An entry going away while we walk is ordinary.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}

		if dirEntry.IsDir() {
			if path == manifestDirectory {
				return fs.SkipDir
			}
			return nil
		}

		if path == manifestPointer {
			return nil
		}

		info, err := dirEntry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("statting %q: %w", path, err)
		}

		entries = append(entries, artifactEntry{relativePath: path, modTime: info.ModTime()})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return entries, nil
}

// reachability is the set of artifact paths the manifests reference, along with the manifests it
// was built from so that a refresh only reads the ones that have appeared since.
type reachability struct {
	paths     map[string]struct{}
	manifests map[string]struct{}
}

func newReachability() *reachability {
	return &reachability{
		paths:     map[string]struct{}{},
		manifests: map[string]struct{}{},
	}
}

// references reports whether any manifest references the entry at relativePath.
func (r *reachability) references(relativePath string) bool {
	_, ok := r.paths[relativePath]

	return ok
}

// refresh brings the set up to date with the manifests on disk. It has to complete for anything to
// be removed, as a partial set would mean removing artifacts that are still referenced, so a
// cancelled context is reported like any other failure.
func (r *reachability) refresh(ctx context.Context, mvccPath string) error {
	manifests, err := os.ReadDir(filepath.Join(mvccPath, manifestDirectory))
	if err != nil {
		return fmt.Errorf("reading manifests: %w", err)
	}

	if len(manifests) == 0 {
		return errNoManifests
	}

	for _, manifest := range manifests {
		if err := ctx.Err(); err != nil {
			return err
		}

		// A directory is not a manifest and references nothing.
		if manifest.IsDir() {
			continue
		}

		if _, ok := r.manifests[manifest.Name()]; ok {
			continue
		}

		parsed, err := readManifest(filepath.Join(mvccPath, manifestDirectory, manifest.Name()))
		if err != nil {
			// Manifest pruning may have removed it since we listed the directory. A
			// manifest that is gone references nothing.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("reading manifest %q: %w", manifest.Name(), err)
		}

		r.manifests[manifest.Name()] = struct{}{}
		for _, path := range parsed.Paths {
			r.paths[path] = struct{}{}
		}
	}

	return nil
}

// deletable narrows entries down to those that are unreferenced and old enough to remove.
func deletable(entries []artifactEntry, minAge time.Duration, reachable *reachability) []artifactEntry {
	cutoff := time.Now().Add(-minAge)

	var candidates []artifactEntry
	for _, entry := range entries {
		if reachable.references(entry.relativePath) {
			continue
		}

		if entry.modTime.Before(cutoff) {
			candidates = append(candidates, entry)
		}
	}

	return candidates
}

// removeStale removes the entries that are still unreferenced and still old enough, giving up when
// a write arrives, when Gitaly is shutting down, or when the budget is spent.
//
// Both are checked again against the refreshed reachability rather than taken from deletable, as a
// manifest published since the candidates were chosen may reference one of them, and an entry may
// have been rewritten. Failing to remove one is not fatal: the next run retries it.
func removeStale(ctx context.Context, mvccPath string, entries []artifactEntry, minAge time.Duration, lock *CollectionLock, reachable *reachability) int {
	logger := log.FromContext(ctx)
	cutoff := time.Now().Add(-minAge)

	removed := 0
	for _, entry := range entries {
		if ctx.Err() != nil || lock.Evicted() {
			break
		}

		if reachable.references(entry.relativePath) {
			continue
		}

		path := filepath.Join(mvccPath, entry.relativePath)

		info, err := os.Stat(path)
		if err != nil {
			// Something else having removed it is ordinary; anything else is not.
			if !errors.Is(err, fs.ErrNotExist) {
				logger.WithError(err).WithField("artifact", entry.relativePath).WarnContext(ctx,
					"could not stat MVCC artifact")
			}
			continue
		}

		if !info.ModTime().Before(cutoff) {
			continue
		}

		if err := os.Remove(path); err != nil {
			logger.WithError(err).WithField("artifact", entry.relativePath).WarnContext(ctx,
				"could not remove MVCC artifact")
			continue
		}

		removed++
	}

	return removed
}

// readManifest reads and parses the manifest at path.
func readManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return ParseManifest(data)
}
