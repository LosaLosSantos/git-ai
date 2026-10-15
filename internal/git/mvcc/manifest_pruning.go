package mvcc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/feder-cr/open_scion/internal/log"
)

// ManifestPruningConfig configures a manifest pruning run.
type ManifestPruningConfig struct {
	// ManifestsToKeep is how many of the most recent manifests are kept regardless of their age.
	// Manifests must be kept for long enough that a client which received a manifest hash from
	// one RPC can still use it in the next.
	ManifestsToKeep uint
	// ManifestMinAge is how old a manifest must be before it may be pruned. It is raised to
	// minEntryAge if it is shorter than that.
	ManifestMinAge time.Duration
	// MaxLockDuration is how long a run may hold the repository lock for. Manifests left over
	// when it runs out are pruned by the next run.
	MaxLockDuration time.Duration
	// MaxAcquireWait is how long a run may wait for the repository lock before giving up.
	MaxAcquireWait time.Duration
}

// ManifestPruningResult reports what a manifest pruning run did.
type ManifestPruningResult struct {
	// Removed is the number of manifests removed.
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

// manifestEntry is an entry of the manifest directory along with the modification time seen while
// scanning it.
type manifestEntry struct {
	name    string
	modTime time.Time
}

// PruneManifests removes the manifests that have fallen out of the retention policy: the
// ManifestsToKeep most recent ones are kept regardless of age, anything younger than
// ManifestMinAge is kept regardless of how many newer ones there are, and the manifest the
// pointer names is kept always, since the repository would be unopenable without it.
//
// Nothing in the manifest directory is recognized by name. Gitaly owns the directory, so an entry
// that is not a manifest is at best a temporary file a download left behind, and the policy is as
// good a way to reclaim one as any.
//
// Which manifests to prune is decided without holding the repository lock, which is taken only to
// delete them, so a run that finds nothing to prune never takes it at all.
//
// The pointer is read again under the lock. Manifests are content-addressed, so a write can
// reproduce one and move the pointer onto a manifest that was past retention when we decided.
//
// The artifacts a pruned manifest referenced are left behind. From then on nothing references
// them, and reclaiming them is a matter of reachability rather than of policy.
func PruneManifests(ctx context.Context, repoPath string, cfg ManifestPruningConfig) (ManifestPruningResult, error) {
	var result ManifestPruningResult

	if cfg.ManifestsToKeep == 0 {
		return result, fmt.Errorf("at least one manifest must be kept")
	}

	if cfg.MaxLockDuration <= 0 {
		return result, fmt.Errorf("no time budget to delete under")
	}

	mvccPath := filepath.Join(repoPath, mvccCacheDir)

	manifests, err := readManifests(filepath.Join(mvccPath, manifestDirectory))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The repository holds no MVCC data yet.
			return result, nil
		}
		return result, fmt.Errorf("scanning manifests: %w", err)
	}

	minAge := max(cfg.ManifestMinAge, minEntryAge)

	obsolete := obsoleteManifests(manifests, minAge, cfg.ManifestsToKeep)
	if len(obsolete) == 0 {
		return result, nil
	}

	lock, waited, skipped, cancelled := acquire(ctx, repoPath, cfg.MaxAcquireWait)
	result.AcquireDuration, result.Skipped, result.Cancelled = waited, skipped, cancelled
	if lock == nil {
		return result, nil
	}
	defer lock.Unlock()

	pointer, err := readPointer(mvccPath)
	if err != nil {
		return result, err
	}
	obsolete = slices.DeleteFunc(obsolete, func(manifest manifestEntry) bool {
		return manifest.name == pointer
	})

	deleteCtx, cancel := context.WithTimeout(ctx, cfg.MaxLockDuration)
	defer cancel()

	result.Removed = removeManifests(deleteCtx, mvccPath, obsolete, minAge, lock)
	result.Evicted = lock.Evicted()
	result.Cancelled = ctx.Err() != nil
	result.LockDuration = lock.HeldFor()

	return result, nil
}

// obsoleteManifests returns the manifests the retention policy no longer keeps alive.
// Recency is judged by modification time, as a manifest's content-addressed name says nothing
// about when it was published.
func obsoleteManifests(manifests []manifestEntry, minAge time.Duration, toKeep uint) []manifestEntry {
	slices.SortFunc(manifests, func(a, b manifestEntry) int { return b.modTime.Compare(a.modTime) })

	cutoff := time.Now().Add(-minAge)

	// Sorted newest first, so the manifests old enough to prune are the tail of the slice.
	// Find where that tail begins. The first toKeep manifests are kept whatever their age,
	// so pruning starts at whichever of the two indexes comes later.
	first, _ := slices.BinarySearchFunc(manifests, cutoff, func(manifest manifestEntry, cutoff time.Time) int {
		return cutoff.Compare(manifest.modTime)
	})

	if uint(first) < toKeep {
		first = int(toKeep)
	}

	if first >= len(manifests) {
		return nil
	}

	return manifests[first:]
}

// removeManifests removes the given manifests, stopping when a write arrives, when Gitaly is
// shutting down, or when the run has held the lock for long enough. Failing to remove one is not
// fatal: the next run retries it.
//
// The age is checked again here rather than taken from the scan. Manifests are content-addressed,
// so a write that publishes one which already exists rewrites that very file, and a manifest that
// was past retention when the run decided may no longer be.
func removeManifests(ctx context.Context, mvccPath string, manifests []manifestEntry, minAge time.Duration, lock *CollectionLock) int {
	logger := log.FromContext(ctx)
	cutoff := time.Now().Add(-minAge)

	removed := 0
	for _, manifest := range manifests {
		if ctx.Err() != nil || lock.Evicted() {
			break
		}

		path := filepath.Join(mvccPath, manifestDirectory, manifest.name)

		info, err := os.Stat(path)
		if err != nil {
			// Something else having removed it is ordinary; anything else is not, and
			// would otherwise be indistinguishable from the manifest being too young.
			if !errors.Is(err, fs.ErrNotExist) {
				logger.WithError(err).WithField("manifest", manifest.name).WarnContext(ctx,
					"could not stat MVCC manifest")
			}
			continue
		}

		if !info.ModTime().Before(cutoff) {
			continue
		}

		if err := os.Remove(path); err != nil {
			logger.WithError(err).WithField("manifest", manifest.name).WarnContext(ctx,
				"could not remove MVCC manifest")
			continue
		}

		removed++
	}

	return removed
}

// readManifests lists the manifest directory along with each entry's modification time.
func readManifests(path string) ([]manifestEntry, error) {
	dirEntries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	entries := make([]manifestEntry, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		// A directory is not a manifest, and unlinking one would fail on every run.
		if dirEntry.IsDir() {
			continue
		}

		info, err := dirEntry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("statting %q: %w", dirEntry.Name(), err)
		}

		entries = append(entries, manifestEntry{name: dirEntry.Name(), modTime: info.ModTime()})
	}

	return entries, nil
}

// readPointer reads the name of the manifest the repository currently points at. An empty name
// means none has been published yet.
func readPointer(mvccPath string) (string, error) {
	pointer, err := os.ReadFile(filepath.Join(mvccPath, manifestPointer))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("reading manifest pointer: %w", err)
	}

	return strings.TrimSpace(string(pointer)), nil
}
