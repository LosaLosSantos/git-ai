package mvcc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// artifactConfig collects eagerly, so that tests only have to age the entries they care about.
func artifactConfig() ArtifactCollectionConfig {
	return ArtifactCollectionConfig{
		ArtifactMinAge:  minEntryAge,
		MaxLockDuration: time.Minute,
	}
}

// artifactCounts strips the timings from a result, so it can be compared for what it did.
func artifactCounts(result ArtifactCollectionResult) ArtifactCollectionResult {
	result.AcquireDuration = 0
	result.LockDuration = 0

	return result
}

func TestCollectArtifacts(t *testing.T) {
	t.Parallel()

	t.Run("removes the artifacts no manifest references", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		live := repo.writeArtifact(reftable("a"), minEntryAge+time.Hour)
		orphan := repo.writeArtifact(reftable("b"), minEntryAge+time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute, live))

		result, err := CollectArtifacts(ctx, repo.path, artifactConfig())
		require.NoError(t, err)
		require.Equal(t, ArtifactCollectionResult{Removed: 1}, artifactCounts(result))

		repo.requireExists(live, true)
		repo.requireExists(orphan, false)
	})

	t.Run("keeps an artifact any manifest still references", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		// Reachability is the union of every manifest on disk, so an artifact only the
		// older one references is still reachable.
		shared := repo.writeArtifact(reftable("a"), minEntryAge+time.Hour)
		repo.writeManifest("older", minEntryAge+time.Hour, shared)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		result, err := CollectArtifacts(ctx, repo.path, artifactConfig())
		require.NoError(t, err)
		require.Equal(t, ArtifactCollectionResult{}, artifactCounts(result))

		repo.requireExists(shared, true)
	})

	t.Run("removes anything unreferenced, whatever it is called and wherever it is", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		// Nothing below the MVCC directory is recognized by name, and the walk descends
		// into whatever directories it finds.
		loose := repo.writeArtifact("pack/not-named-like-a-pack", minEntryAge+time.Hour)
		nested := repo.writeArtifactUnder("pack/nested", "leftover", minEntryAge+time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		result, err := CollectArtifacts(ctx, repo.path, artifactConfig())
		require.NoError(t, err)
		require.Equal(t, ArtifactCollectionResult{Removed: 2}, artifactCounts(result))

		repo.requireExists(loose, false)
		repo.requireExists(nested, false)
	})

	t.Run("keeps a fetch in progress", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		// A fetch writes to a temporary file next to the artifact it is fetching, and it
		// outlives the RPC that started it, so its age is all that keeps it from being
		// removed.
		fetching := repo.writeArtifact(reftable("a")+"1234567890", minEntryAge-time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		cfg := artifactConfig()
		cfg.ArtifactMinAge = time.Minute

		result, err := CollectArtifacts(ctx, repo.path, cfg)
		require.NoError(t, err)
		require.Equal(t, ArtifactCollectionResult{}, artifactCounts(result))

		repo.requireExists(fetching, true)
	})

	t.Run("never touches the manifest directory or the pointer", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		// Both are past the age floor and neither is referenced by a manifest, so only
		// leaving them out of the walk keeps them.
		repo.writeManifest("older", minEntryAge+time.Hour)
		current := repo.writeManifest("current", minEntryAge+time.Hour)
		repo.setPointer(current)
		repo.age(manifestPointer, minEntryAge+time.Hour)

		result, err := CollectArtifacts(ctx, repo.path, artifactConfig())
		require.NoError(t, err)
		require.Equal(t, ArtifactCollectionResult{}, artifactCounts(result))

		repo.requireExists(repo.manifestPath("older"), true)
		repo.requireExists(manifestPointer, true)
	})

	t.Run("stops when a manifest cannot be read", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		orphan := repo.writeArtifact(reftable("a"), minEntryAge+time.Hour)
		repo.writeFile(repo.manifestPath("current"), []byte("not a manifest"), time.Minute)
		repo.setPointer(manifestNameFor("current"))

		_, err := CollectArtifacts(ctx, repo.path, artifactConfig())
		require.ErrorContains(t, err, "reading manifest")

		repo.requireExists(orphan, true)
	})

	t.Run("does nothing before the first manifest is published", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		orphan := repo.writeArtifact(reftable("a"), minEntryAge+time.Hour)

		result, err := CollectArtifacts(ctx, repo.path, artifactConfig())
		require.NoError(t, err)
		require.Equal(t, ArtifactCollectionResult{}, artifactCounts(result))

		repo.requireExists(orphan, true)
	})

	t.Run("does nothing for a repository without MVCC data", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)

		result, err := CollectArtifacts(ctx, testhelper.TempDir(t), artifactConfig())
		require.NoError(t, err)
		require.Equal(t, ArtifactCollectionResult{}, artifactCounts(result))
	})

	t.Run("does not lock the repository when there is nothing to collect", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		live := repo.writeArtifact(reftable("a"), minEntryAge+time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute, live))

		unlockWrite, err := LockRepositoryForWrite(ctx, repo.path)
		require.NoError(t, err)
		defer unlockWrite()

		result, err := CollectArtifacts(ctx, repo.path, artifactConfig())
		require.NoError(t, err)
		require.Equal(t, ArtifactCollectionResult{}, artifactCounts(result))
	})

	t.Run("gives up when the repository keeps being written to", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		orphan := repo.writeArtifact(reftable("a"), minEntryAge+time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		unlockWrite, err := LockRepositoryForWrite(ctx, repo.path)
		require.NoError(t, err)
		defer unlockWrite()

		// Long enough to retry rather than to give up on the first attempt.
		cfg := artifactConfig()
		cfg.MaxAcquireWait = 10 * acquireBackoff

		result, err := CollectArtifacts(ctx, repo.path, cfg)
		require.NoError(t, err)
		require.Equal(t, ArtifactCollectionResult{Skipped: true}, artifactCounts(result))

		repo.requireExists(orphan, true)
	})

	t.Run("stops deleting once it has spent its time budget", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		orphan := repo.writeArtifact(reftable("a"), minEntryAge+time.Hour)
		repo.setPointer(repo.writeManifest("current", time.Minute))

		cfg := artifactConfig()
		cfg.MaxLockDuration = time.Nanosecond

		result, err := CollectArtifacts(ctx, repo.path, cfg)
		require.NoError(t, err)
		require.Equal(t, ArtifactCollectionResult{}, artifactCounts(result))

		repo.requireExists(orphan, true)
	})

	t.Run("refuses to run without a time budget", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)

		cfg := artifactConfig()
		cfg.MaxLockDuration = 0

		_, err := CollectArtifacts(ctx, testhelper.TempDir(t), cfg)
		require.ErrorContains(t, err, "no time budget to delete under")
	})
}

// TestReachability covers refreshing the set, which is what a run does under the lock instead of
// reading every manifest again.
func TestReachability(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := newMVCCRepo(t)

	first := repo.writeArtifact(reftable("a"), 0)
	repo.writeManifest("first", time.Minute, first)

	reachable := newReachability()
	require.NoError(t, reachable.refresh(ctx, repo.mvccPath))
	require.True(t, reachable.references(first))
	require.Equal(t, map[string]struct{}{manifestNameFor("first"): {}}, reachable.manifests)

	second := repo.writeArtifact(reftable("b"), 0)
	repo.writeManifest("second", time.Minute, second)

	require.NoError(t, reachable.refresh(ctx, repo.mvccPath))
	require.True(t, reachable.references(second))
	require.Len(t, reachable.manifests, 2)

	// Manifest pruning runs without excluding the scan, so a manifest can go away between
	// being listed and being read. A dangling symlink is listed and then fails to open the
	// same way.
	require.NoError(t, os.Symlink(repo.manifestPath("gone"), filepath.Join(repo.mvccPath, repo.manifestPath("dangling"))))
	require.NoError(t, reachable.refresh(ctx, repo.mvccPath))
	require.True(t, reachable.references(first))

	// A run that is out of time stops without a complete picture, which is what keeps it from
	// deleting anything.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, newReachability().refresh(cancelled, repo.mvccPath), context.Canceled)
}

func TestRemoveStale(t *testing.T) {
	t.Parallel()

	t.Run("stops when the context is cancelled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(testhelper.Context(t))
		repo := newMVCCRepo(t)

		candidates := []artifactEntry{{relativePath: repo.writeArtifact(reftable("a"), minEntryAge+time.Hour)}}

		lock := TryLockRepositoryForCollection(repo.path)
		require.NotNil(t, lock)
		defer lock.Unlock()

		cancel()

		require.Zero(t, removeStale(ctx, repo.mvccPath, candidates, time.Hour, lock, newReachability()))
		repo.requireExists(reftable("a"), true)
	})

	t.Run("stops when a write arrives", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		repo := newMVCCRepo(t)

		var candidates []artifactEntry
		for i := range 10 {
			candidates = append(candidates, artifactEntry{
				relativePath: repo.writeArtifact(numberedReftable(i), minEntryAge+time.Hour),
			})
		}

		lock := TryLockRepositoryForCollection(repo.path)
		require.NotNil(t, lock)

		unlockWrite := lockRepositoryForWriteAsync(t, ctx, repo.path)
		require.Eventually(t, lock.Evicted, time.Minute, time.Millisecond)

		require.Zero(t, removeStale(ctx, repo.mvccPath, candidates, time.Hour, lock, newReachability()))
		repo.requireExists(numberedReftable(0), true)

		lock.Unlock()
		unlockWrite()
	})
}

// artifactShape is a repository to run the collection benchmark against.
type artifactShape struct {
	// manifests is how many manifests are on disk. Every one of them has to be read to know
	// what is reachable.
	manifests int
	// artifactsPerManifest is how many artifacts a single manifest references.
	artifactsPerManifest int
	// garbage is how many unreferenced entries are to be removed.
	garbage int
}

func (shape artifactShape) desc() string {
	return fmt.Sprintf("%d manifests, %d artifacts each, %d garbage",
		shape.manifests, shape.artifactsPerManifest, shape.garbage)
}

// artifactShapes move one variable at a time around a common base, so that what a run costs can be
// attributed rather than only observed.
var artifactShapes = []artifactShape{
	{manifests: 50, artifactsPerManifest: 50, garbage: 50},
	{manifests: 500, artifactsPerManifest: 200, garbage: 500},
	{manifests: 2000, artifactsPerManifest: 200, garbage: 500},
	{manifests: 500, artifactsPerManifest: 1000, garbage: 500},
	{manifests: 500, artifactsPerManifest: 200, garbage: 2000},
}

// BenchmarkCollectArtifacts measures a run end to end, reading the manifests included.
func BenchmarkCollectArtifacts(b *testing.B) {
	ctx := testhelper.Context(b)

	for _, shape := range artifactShapes {
		b.Run(shape.desc(), func(b *testing.B) {
			repo := newMVCCRepo(b)

			paths := make([]string, 0, shape.artifactsPerManifest)
			for i := range shape.artifactsPerManifest {
				paths = append(paths, repo.writeArtifact(numberedReftable(i), minEntryAge+time.Hour))
			}

			for i := range shape.manifests {
				repo.writeManifest(fmt.Sprintf("manifest-%05d", i), time.Duration(i)*time.Minute, paths...)
			}
			repo.setPointer(manifestNameFor("manifest-00000"))

			var runs int
			var locked time.Duration

			for b.Loop() {
				b.StopTimer()
				// Every iteration consumes the entries it collects, so they have to be
				// written back.
				for i := range shape.garbage {
					repo.writeArtifact(numberedReftable(shape.artifactsPerManifest+i), minEntryAge+time.Hour)
				}
				b.StartTimer()

				result, err := CollectArtifacts(ctx, repo.path, artifactConfig())
				require.NoError(b, err)
				require.Equal(b, shape.garbage, result.Removed)

				runs++
				locked += result.LockDuration
			}

			// Only the deletions run under the lock, which is what a write can end up
			// waiting for, so it is reported separately from the run as a whole.
			b.ReportMetric(float64(locked.Nanoseconds())/float64(runs), "ns/locked")
		})
	}
}
