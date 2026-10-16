package mvcc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// testConfig prunes eagerly, so that tests only have to age the manifests they care about.
func testConfig() ManifestPruningConfig {
	return ManifestPruningConfig{
		ManifestsToKeep: 1,
		ManifestMinAge:  minEntryAge,
		MaxLockDuration: time.Minute,
	}
}

func TestPruneManifests(t *testing.T) {
	t.Parallel()

	t.Run("prunes manifests past retention and leaves their artifacts", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		artifact := repo.writeArtifact(reftable("a"), minEntryAge+time.Hour)
		repo.writeManifest("obsolete", minEntryAge+time.Hour, artifact)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		result, err := PruneManifests(ctx, repo.path, testConfig())
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{Removed: 1}, counts(result))

		repo.requireExists(repo.manifestPath("obsolete"), false)
		repo.requireExists(repo.manifestPath("current"), true)
		// Nothing references it anymore, but reclaiming it is not this run's business.
		repo.requireExists(artifact, true)
	})

	t.Run("keeps the most recent manifests regardless of their age", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		repo.writeManifest("older", minEntryAge+2*time.Hour)
		repo.writeManifest("newer", minEntryAge+time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		cfg := testConfig()
		cfg.ManifestsToKeep = 2

		result, err := PruneManifests(ctx, repo.path, cfg)
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{Removed: 1}, counts(result))

		repo.requireExists(repo.manifestPath("older"), false)
		repo.requireExists(repo.manifestPath("newer"), true)
	})

	t.Run("keeps manifests that are not old enough", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		// Past the count the policy keeps alive, but younger than the minimum age, which
		// is what a client that is still using a manifest is protected by.
		repo.writeManifest("recent", time.Minute)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		result, err := PruneManifests(ctx, repo.path, testConfig())
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{}, counts(result))

		repo.requireExists(repo.manifestPath("recent"), true)
	})

	t.Run("never removes the manifest the pointer names", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		// The policy would prune this one, but the repository would be unopenable without
		// it.
		current := repo.writeManifest("current", minEntryAge+time.Hour)
		repo.writeManifest("newer", time.Minute)
		repo.setPointer(current)

		result, err := PruneManifests(ctx, repo.path, testConfig())
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{}, counts(result))

		repo.requireExists(repo.manifestPath("current"), true)
	})

	t.Run("removes a manifest past retention without reading it", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		// Pruning is a matter of policy, so an unreadable manifest goes like any other
		// once it is past retention.
		repo.writeFile(repo.manifestPath("obsolete"), []byte("not a manifest"), minEntryAge+time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		result, err := PruneManifests(ctx, repo.path, testConfig())
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{Removed: 1}, counts(result))

		repo.requireExists(repo.manifestPath("obsolete"), false)
	})

	t.Run("prunes whatever else is past retention, whatever it is called", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		// What a crashed manifest download leaves behind. Nothing in the directory is
		// recognized by name, so the policy reclaims it like it does a manifest.
		leftover := repo.manifestPath("current") + "1234567890"
		repo.writeFile(leftover, []byte("half a manifest"), minEntryAge+time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		result, err := PruneManifests(ctx, repo.path, testConfig())
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{Removed: 1}, counts(result))

		repo.requireExists(leftover, false)
	})

	t.Run("keeps a manifest download in progress", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		// A download writes to a temporary file in the manifest directory, and a read
		// downloading one does not hold the repository lock, so its age is all that keeps
		// it from being pruned.
		downloading := repo.manifestPath("current") + "1234567890"
		repo.writeFile(downloading, []byte("half a manifest"), minEntryAge-time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		cfg := testConfig()
		cfg.ManifestMinAge = time.Minute

		result, err := PruneManifests(ctx, repo.path, cfg)
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{}, counts(result))

		repo.requireExists(downloading, true)
	})

	t.Run("does not lock the repository when there is nothing to prune", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		repo.setPointer(repo.writeManifest("current", time.Minute))

		// A write holds the repository, so a run that needed the lock would report having
		// been skipped.
		unlockWrite, err := LockRepositoryForWrite(ctx, repo.path)
		require.NoError(t, err)
		defer unlockWrite()

		result, err := PruneManifests(ctx, repo.path, testConfig())
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{}, counts(result))
	})

	t.Run("gives up when the repository keeps being written to", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		repo.writeManifest("obsolete", minEntryAge+time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		unlockWrite, err := LockRepositoryForWrite(ctx, repo.path)
		require.NoError(t, err)
		defer unlockWrite()

		result, err := PruneManifests(ctx, repo.path, testConfig())
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{Skipped: true}, counts(result))

		repo.requireExists(repo.manifestPath("obsolete"), true)
	})

	t.Run("waits for a write to finish", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		repo.writeManifest("obsolete", minEntryAge+time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		unlockWrite, err := LockRepositoryForWrite(ctx, repo.path)
		require.NoError(t, err)
		go func() {
			time.Sleep(10 * acquireBackoff)
			unlockWrite()
		}()

		cfg := testConfig()
		cfg.MaxAcquireWait = time.Minute

		result, err := PruneManifests(ctx, repo.path, cfg)
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{Removed: 1}, counts(result))

		repo.requireExists(repo.manifestPath("obsolete"), false)
	})

	t.Run("stops deleting once it has spent its time budget", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		repo.writeManifest("obsolete", minEntryAge+time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		cfg := testConfig()
		cfg.MaxLockDuration = time.Nanosecond

		result, err := PruneManifests(ctx, repo.path, cfg)
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{}, counts(result))

		repo.requireExists(repo.manifestPath("obsolete"), true)
	})

	t.Run("does nothing for a repository without MVCC data", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)

		result, err := PruneManifests(ctx, testhelper.TempDir(t), testConfig())
		require.NoError(t, err)
		require.Equal(t, ManifestPruningResult{}, counts(result))
	})

	t.Run("refuses to keep no manifests at all", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)

		cfg := testConfig()
		cfg.ManifestsToKeep = 0

		_, err := PruneManifests(ctx, testhelper.TempDir(t), cfg)
		require.ErrorContains(t, err, "at least one manifest must be kept")
	})

	t.Run("refuses to run without a time budget", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)

		cfg := testConfig()
		cfg.MaxLockDuration = 0

		_, err := PruneManifests(ctx, testhelper.TempDir(t), cfg)
		require.ErrorContains(t, err, "no time budget to delete under")
	})
}

// TestRemoveManifests covers stopping mid-deletion, which is what bounds how long a write waits
// for a run. Both cases need the lock already held, which is why they drive removeManifests rather
// than a whole run.
func TestRemoveManifests(t *testing.T) {
	t.Parallel()

	t.Run("stops when the context is cancelled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(testhelper.Context(t))
		repo := newMVCCRepo(t)

		obsolete := []manifestEntry{{name: repo.writeManifest("obsolete", minEntryAge+time.Hour)}}

		lock := TryLockRepositoryForCollection(repo.path)
		require.NotNil(t, lock)
		defer lock.Unlock()

		cancel()

		require.Zero(t, removeManifests(ctx, repo.mvccPath, obsolete, minEntryAge, lock))
		repo.requireExists(repo.manifestPath("obsolete"), true)
	})

	t.Run("keeps a manifest that was republished while waiting for the lock", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		// The scan found it past retention, but a write has recreated it since: manifests
		// are content-addressed, so publishing one that already exists rewrites this very
		// file.
		obsolete := []manifestEntry{{name: repo.writeManifest("obsolete", minEntryAge+time.Hour)}}
		repo.writeManifest("obsolete", time.Minute)

		lock := TryLockRepositoryForCollection(repo.path)
		require.NotNil(t, lock)
		defer lock.Unlock()

		require.Zero(t, removeManifests(ctx, repo.mvccPath, obsolete, minEntryAge, lock))
		repo.requireExists(repo.manifestPath("obsolete"), true)
	})

	t.Run("stops when a write arrives", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		var obsolete []manifestEntry
		for i := range 10 {
			obsolete = append(obsolete, manifestEntry{
				name: repo.writeManifest(fmt.Sprintf("obsolete-%d", i), minEntryAge+time.Hour),
			})
		}

		lock := TryLockRepositoryForCollection(repo.path)
		require.NotNil(t, lock)

		unlockWrite := lockRepositoryForWriteAsync(t, ctx, repo.path)
		require.Eventually(t, lock.Evicted, time.Minute, time.Millisecond)

		require.Zero(t, removeManifests(ctx, repo.mvccPath, obsolete, minEntryAge, lock))
		repo.requireExists(repo.manifestPath("obsolete-0"), true)

		lock.Unlock()
		unlockWrite()
	})
}

// benchmarkShape is a repository to run the pruning benchmark against.
type benchmarkShape struct {
	// retained is how many manifests the retention policy keeps alive. Every run lists them to
	// work out which manifests have fallen out of the policy.
	retained int
	// obsolete is how many manifests have fallen out of the policy and are to be removed.
	obsolete int
}

// desc names the shape after the two numbers a run's cost depends on.
func (shape benchmarkShape) desc() string {
	return fmt.Sprintf("%d retained, %d obsolete", shape.retained, shape.obsolete)
}

// benchmarkShapes move one variable at a time around a common base, so that what a run costs can
// be attributed rather than only observed.
var benchmarkShapes = []benchmarkShape{
	{retained: 50, obsolete: 50},
	{retained: 500, obsolete: 50},
	{retained: 500, obsolete: 500},
	{retained: 2000, obsolete: 500},
	{retained: 8000, obsolete: 500},
	{retained: 500, obsolete: 2000},
	{retained: 500, obsolete: 8000},
}

// BenchmarkPruneManifests measures a run end to end, including the part that is not under the lock.
func BenchmarkPruneManifests(b *testing.B) {
	ctx := testhelper.Context(b)

	for _, shape := range benchmarkShapes {
		b.Run(shape.desc(), func(b *testing.B) {
			repo := newMVCCRepo(b)

			for i := range shape.retained {
				repo.writeManifest(fmt.Sprintf("retained-%05d", i), time.Duration(i)*time.Minute)
			}
			repo.setPointer(manifestNameFor("retained-00000"))

			cfg := testConfig()
			cfg.ManifestsToKeep = uint(shape.retained)

			for b.Loop() {
				b.StopTimer()
				// Every iteration consumes the manifests it prunes, so they have to be
				// written back. They are older than any retained manifest, so these are
				// the ones the policy drops.
				for j := range shape.obsolete {
					repo.writeManifest(fmt.Sprintf("obsolete-%05d", j), 30*24*time.Hour)
				}
				b.StartTimer()

				result, err := PruneManifests(ctx, repo.path, cfg)
				require.NoError(b, err)
				require.Equal(b, shape.obsolete, result.Removed)
			}
		})
	}
}
