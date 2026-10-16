package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/log"
)

// The names the two runs report themselves under.
const (
	taskPruneManifests   = "mvcc-prune-manifests"
	taskCollectArtifacts = "mvcc-collect-artifacts"
)

// mvccRun is what a garbage collection run did, in the shape the metrics need.
type mvccRun struct {
	task         string
	filetype     string
	removed      int
	skipped      bool
	evicted      bool
	lockDuration time.Duration
}

// RunMVCCGarbageCollection prunes the manifests that have fallen out of the retention policy and
// then removes the artifacts none of the remaining manifests reference. Collecting after pruning
// reclaims the artifacts of a manifest pruned in the same round. It does nothing for a repository
// on another reference backend.
//
// It must not be called while an MVCC cache is open for the repository. A cache holds the
// repository lock for as long as the write it belongs to, so both runs would find the repository
// busy and do nothing. That is why this is not part of OptimizeRepository, which runs inside a
// cache of its own.
func (m *RepositoryManager) RunMVCCGarbageCollection(ctx context.Context, repo *localrepo.Repo, cfg housekeeping.MVCCConfig) error {
	backend, err := repo.ReferenceBackend(ctx)
	if err != nil {
		return fmt.Errorf("detect reference backend: %w", err)
	}

	if backend.Name != git.ReferenceBackendMVCC.Name {
		return nil
	}

	repoPath, err := repo.Path(ctx)
	if err != nil {
		return fmt.Errorf("repository path: %w", err)
	}

	finishManifests := m.metrics.ReportTaskLatency(taskPruneManifests, "apply")
	manifests, manifestsErr := mvcc.PruneManifests(ctx, repoPath, mvcc.ManifestPruningConfig{
		ManifestsToKeep: cfg.ManifestsToKeep,
		ManifestMinAge:  cfg.ManifestMinAge.Duration(),
		MaxLockDuration: cfg.MaxLockDuration.Duration(),
		MaxAcquireWait:  cfg.MaxAcquireWait.Duration(),
	})
	finishManifests()
	m.reportMVCCRun(mvccRun{
		task:         taskPruneManifests,
		filetype:     "manifest",
		removed:      manifests.Removed,
		skipped:      manifests.Skipped,
		evicted:      manifests.Evicted,
		lockDuration: manifests.LockDuration,
	}, manifestsErr)

	finishArtifacts := m.metrics.ReportTaskLatency(taskCollectArtifacts, "apply")
	artifacts, artifactsErr := mvcc.CollectArtifacts(ctx, repoPath, mvcc.ArtifactCollectionConfig{
		ArtifactMinAge:  cfg.ArtifactMinAge.Duration(),
		MaxLockDuration: cfg.MaxLockDuration.Duration(),
		MaxAcquireWait:  cfg.MaxAcquireWait.Duration(),
	})
	finishArtifacts()
	m.reportMVCCRun(mvccRun{
		task:         taskCollectArtifacts,
		filetype:     "artifact",
		removed:      artifacts.Removed,
		skipped:      artifacts.Skipped,
		evicted:      artifacts.Evicted,
		lockDuration: artifacts.LockDuration,
	}, artifactsErr)

	m.logger.WithFields(log.Fields{
		"storage_name":               repo.GetStorageName(),
		"relative_path":              repo.GetRelativePath(),
		"manifests_removed":          manifests.Removed,
		"manifests_skipped":          manifests.Skipped,
		"manifests_evicted":          manifests.Evicted,
		"manifests_lock_duration_ms": manifests.LockDuration.Milliseconds(),
		"artifacts_removed":          artifacts.Removed,
		"artifacts_skipped":          artifacts.Skipped,
		"artifacts_evicted":          artifacts.Evicted,
		"artifacts_lock_duration_ms": artifacts.LockDuration.Milliseconds(),
	}).InfoContext(ctx, "collected MVCC garbage")

	return errors.Join(manifestsErr, artifactsErr)
}

// reportMVCCRun records what a garbage collection run did. A run that could not lock the
// repository, and one that handed it back to a write, are reported apart from one that ran to the
// end: being locked out or evicted repeatedly is the failure mode to watch for.
func (m *RepositoryManager) reportMVCCRun(run mvccRun, err error) {
	status := "success"
	switch {
	case err != nil:
		status = "failure"
	case run.skipped:
		status = "skipped"
	case run.evicted:
		status = "evicted"
	}

	m.metrics.TasksTotal.WithLabelValues(run.task, status).Inc()
	m.metrics.PrunedFilesTotal.WithLabelValues(run.filetype).Add(float64(run.removed))

	// The task latency times a whole run, most of which is spent working out what to remove
	// without the lock. A run that never took the lock has nothing to record.
	if run.lockDuration > 0 {
		m.metrics.MVCCLockDuration.WithLabelValues(run.task).Observe(run.lockDuration.Seconds())
	}
}
