// Package maintenance runs the background checkpoints and garbage collection
// of the storage core (doc/graft/PLAN7.md, "Background maintenance"). It is
// off by default and runs in authoritative mode only; the graft storage
// commands do the same work on demand.
//
// Every interval it collects the removed repositories of the node's storages,
// then, for every repository of those storages that this node has a current
// local copy of, it takes the repository's maintenance lease (skipping a
// repository whose lease another process holds), checkpoints the repository
// when enough changesets or pack bytes follow its newest checkpoint, and
// collects its dead packs. The lease only keeps two nodes from doing the same
// work at once: the checkpoint and the collector are correct without it.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/checkpoint"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gc"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// changesetPage is how many changesets are read at once to sum pack bytes.
const changesetPage = 500

// releaseTimeout bounds the release of a lease after a run ended.
const releaseTimeout = 10 * time.Second

// Steps, the label of graft_storage_maintenance_failures_total.
const (
	stepCollectRemoved = "collect_removed"
	stepList           = "list"
	stepLease          = "lease"
	stepCheckpoint     = "checkpoint"
	stepCollect        = "collect"
)

// Config is the configuration of the background maintenance.
type Config struct {
	// Interval is the time between two runs.
	Interval time.Duration
	// CheckpointChangesets checkpoints a repository once this many changesets
	// follow its newest checkpoint; 0 never checkpoints by count.
	CheckpointChangesets uint64
	// CheckpointPackBytes checkpoints a repository once the packs of the
	// changesets after its newest checkpoint hold this many bytes; 0 never
	// checkpoints by size.
	CheckpointPackBytes int64
	// LeaseTTL is how long a run holds a repository's lease before renewing it.
	LeaseTTL time.Duration
}

// Runner runs the background maintenance of one node.
type Runner struct {
	core      *corestore.Core
	cp        *checkpoint.Checkpointer
	collector *gc.Collector
	fresh     corestore.Freshener
	logger    log.Logger
	cfg       Config
	storages  []string
	// holder names this process in the leases: the node ID and the process ID,
	// so that two processes of one node (a graceful upgrade) do not both hold a
	// lease.
	holder string

	runs        *prometheus.CounterVec
	checkpoints prometheus.Counter
	packs       prometheus.Counter
	bytes       prometheus.Counter
	skipped     *prometheus.CounterVec
	failures    *prometheus.CounterVec
}

var _ prometheus.Collector = (*Runner)(nil)

// LeaseHolder names the background maintenance of this process in the
// maintenance leases of core: the node ID and the process ID, so that two
// processes of one node (a graceful upgrade) never both hold a lease.
func LeaseHolder(core *corestore.Core) string {
	return fmt.Sprintf("%s/%d", core.NodeID, os.Getpid())
}

// requests numbers the on-demand maintenance operations of the process.
var requests atomic.Uint64

// RequestLeaseHolder names one on-demand maintenance operation of this process
// (a StorageCoreService RPC): a holder of its own, so that it excludes the
// background runs and the other operations.
func RequestLeaseHolder(core *corestore.Core) string {
	return fmt.Sprintf("%s/request-%d", LeaseHolder(core), requests.Add(1))
}

// New returns the runner of core for the node's storages.
func New(core *corestore.Core, cp *checkpoint.Checkpointer, collector *gc.Collector, fresh corestore.Freshener, logger log.Logger, cfg Config, storages []string) *Runner {
	r := &Runner{
		core:      core,
		cp:        cp,
		collector: collector,
		fresh:     fresh,
		logger:    logger,
		cfg:       cfg,
		storages:  storages,
		holder:    LeaseHolder(core),
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graft_storage_maintenance_runs_total",
			Help: "Runs of the background maintenance of the storage core, by result (ok, error: at least one step failed).",
		}, []string{"result"}),
		checkpoints: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_maintenance_checkpoints_total",
			Help: "Checkpoints created by the background maintenance.",
		}),
		packs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_maintenance_packs_deleted_total",
			Help: "Bucket objects deleted by the background garbage collection.",
		}),
		bytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_maintenance_bytes_reclaimed_total",
			Help: "Bytes of the bucket objects deleted by the background garbage collection.",
		}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graft_storage_maintenance_skipped_total",
			Help: "Repositories the background maintenance skipped, by reason (no_local_copy, lease_held).",
		}, []string{"reason"}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graft_storage_maintenance_failures_total",
			Help: "Failed steps of the background maintenance, by step (collect_removed, list, lease, checkpoint, collect).",
		}, []string{"step"}),
	}
	for _, result := range []string{"ok", "error"} {
		r.runs.WithLabelValues(result)
	}
	for _, reason := range []string{"no_local_copy", "lease_held"} {
		r.skipped.WithLabelValues(reason)
	}
	for _, step := range []string{stepCollectRemoved, stepList, stepLease, stepCheckpoint, stepCollect} {
		r.failures.WithLabelValues(step)
	}
	return r
}

// Start runs the maintenance every interval, the first time one interval after
// the start, until stop is called; stop waits for a run in progress to end
// (its context is cancelled).
func (r *Runner) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(r.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			_ = r.RunOnce(ctx)
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// RunOnce runs the maintenance once. A failure of one repository or one step
// does not stop the others; the returned error joins them.
func (r *Runner) RunOnce(ctx context.Context) error {
	var errs []error
	fail := func(step string, err error, fields log.Fields) {
		r.failures.WithLabelValues(step).Inc()
		r.logger.WithError(err).WithFields(fields).WithField("step", step).Error("storage core: background maintenance step failed")
		errs = append(errs, err)
	}

	for _, storageName := range r.storages {
		report, err := r.collector.CollectRemoved(ctx, storageName, false)
		if err != nil {
			fail(stepCollectRemoved, err, log.Fields{"storage": storageName})
			continue
		}
		r.count(report)
	}

	for _, storageName := range r.storages {
		repos, err := r.core.Meta.ListRepositories(ctx, storageName)
		if err != nil {
			fail(stepList, err, log.Fields{"storage": storageName})
			continue
		}
		for _, record := range repos {
			if ctx.Err() != nil {
				errs = append(errs, ctx.Err())
				break
			}
			if step, err := r.maintain(ctx, record); err != nil {
				fail(step, err, log.Fields{"repository": record.Key.String(), "repository_id": record.ID})
			}
		}
	}

	result := "ok"
	if len(errs) > 0 {
		result = "error"
	}
	r.runs.WithLabelValues(result).Inc()
	return errors.Join(errs...)
}

// maintain checkpoints and collects one repository under its lease. It returns
// the step that failed with the error.
func (r *Runner) maintain(ctx context.Context, record corestore.Repository) (string, error) {
	repo := &gitalypb.Repository{StorageName: record.Key.Storage, RelativePath: record.Key.RelativePath}

	wm, ok, err := r.fresh.Applied(ctx, repo)
	if err != nil {
		return stepList, fmt.Errorf("read the local watermark: %w", err)
	}
	if !ok || wm.RepoID != record.ID {
		r.skipped.WithLabelValues("no_local_copy").Inc()
		return "", nil
	}

	acquired, err := r.core.Meta.AcquireMaintenanceLease(ctx, record.ID, r.holder, r.cfg.LeaseTTL)
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return "", nil
	}
	if err != nil {
		return stepLease, err
	}
	if !acquired {
		r.skipped.WithLabelValues("lease_held").Inc()
		return "", nil
	}

	leaseCtx, release := HoldLease(ctx, r.core, r.logger, record.ID, r.holder, r.cfg.LeaseTTL, func() {
		r.failures.WithLabelValues(stepLease).Inc()
	})
	defer release()

	due, err := r.checkpointDue(leaseCtx, record)
	if err != nil {
		return stepCheckpoint, err
	}
	if due {
		_, created, err := r.cp.Create(leaseCtx, repo)
		if err != nil {
			return stepCheckpoint, err
		}
		if created {
			r.checkpoints.Inc()
		}
	}

	report, err := r.collector.CollectRepository(leaseCtx, record.ID, false)
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return "", nil
	}
	if err != nil {
		return stepCollect, err
	}
	r.count(report)
	return "", nil
}

// HoldLease keeps the maintenance lease of repoID, which holder acquired with
// time to live ttl, for the work done under the returned context: it renews the
// lease every third of ttl, and when the lease is lost it calls lost and
// cancels the context (another process may then do the same work, which is
// wasted but correct). release stops the renewal, waits for it, and releases
// the lease; the background maintenance and the CollectGarbage request use it
// alike.
func HoldLease(ctx context.Context, core *corestore.Core, logger log.Logger, repoID int64, holder string, ttl time.Duration, lost func()) (context.Context, func()) {
	leaseCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
			}
			held, err := core.Meta.RenewMaintenanceLease(leaseCtx, repoID, holder, ttl)
			if leaseCtx.Err() != nil {
				return
			}
			if err != nil || !held {
				logger.WithError(err).WithField("repository_id", repoID).WithField("held", held).
					Warn("storage core: the maintenance lease was lost, the work on the repository stops")
				lost()
				cancel()
				return
			}
		}
	}()

	return leaseCtx, func() {
		cancel()
		<-done
		releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancelRelease()
		if err := core.Meta.ReleaseMaintenanceLease(releaseCtx, repoID, holder); err != nil {
			logger.WithError(err).WithField("repository_id", repoID).Warn("storage core: release the maintenance lease; it expires by itself")
		}
	}
}

// checkpointDue reports whether the repository needs a checkpoint: enough
// changesets, or enough pack bytes, follow its newest checkpoint.
func (r *Runner) checkpointDue(ctx context.Context, record corestore.Repository) (bool, error) {
	if r.cfg.CheckpointChangesets == 0 && r.cfg.CheckpointPackBytes == 0 {
		return false, nil
	}

	var base uint64
	newest, ok, err := r.core.Meta.NewestCheckpoint(ctx, record.ID)
	if err != nil {
		return false, err
	}
	if ok {
		base = newest.Seq
	}
	if record.HeadSeq <= base {
		return false, nil
	}
	if r.cfg.CheckpointChangesets > 0 && record.HeadSeq-base >= r.cfg.CheckpointChangesets {
		return true, nil
	}
	if r.cfg.CheckpointPackBytes == 0 {
		return false, nil
	}

	// Fewer changesets than the count threshold follow the checkpoint (or the
	// count is off): their pack bytes decide, read a page at a time and only
	// until the threshold is reached.
	var total int64
	for after := base; after < record.HeadSeq; {
		upTo := min(after+changesetPage, record.HeadSeq)
		changesets, err := r.core.Meta.ListChangesets(ctx, record.ID, after, upTo)
		if err != nil {
			return false, err
		}
		for _, cs := range changesets {
			total += cs.PackBytes
			if total >= r.cfg.CheckpointPackBytes {
				return true, nil
			}
		}
		after = upTo
	}
	return false, nil
}

// count adds a collection report to the metrics.
func (r *Runner) count(report gc.Report) {
	r.packs.Add(float64(report.PacksDeleted))
	r.bytes.Add(float64(report.BytesReclaimed))
}

// Describe implements prometheus.Collector.
func (r *Runner) Describe(descs chan<- *prometheus.Desc) {
	r.runs.Describe(descs)
	r.checkpoints.Describe(descs)
	r.packs.Describe(descs)
	r.bytes.Describe(descs)
	r.skipped.Describe(descs)
	r.failures.Describe(descs)
}

// Collect implements prometheus.Collector.
func (r *Runner) Collect(metrics chan<- prometheus.Metric) {
	r.runs.Collect(metrics)
	r.checkpoints.Collect(metrics)
	r.packs.Collect(metrics)
	r.bytes.Collect(metrics)
	r.skipped.Collect(metrics)
	r.failures.Collect(metrics)
}
