// Package gc reclaims the packs of the storage core that no reader will ever
// need again (doc/graft/PLAN7.md, "Pack GC" and "Removed repositories",
// binding):
//   - the changeset packs covered by a checkpoint older than gc_grace (C_gc,
//     GC BOUNDARY rule) and the packs of the checkpoints superseded by it;
//   - the objects under the repository's pack prefix that no row references,
//     once the collector itself observed them unreferenced at least gc_grace
//     earlier on the database clock (PACK OWNERSHIP RULE);
//   - every object of a removed repository, once its tombstone is gc_grace
//     old.
//
// Every decision of a run comes from one database snapshot (GCSnapshot) and the
// proofs of PLAN7; the bucket's times are never read. Deleting a missing key is
// success and observations are recorded with "do nothing on conflict", so a
// run that crashed, or two runs that overlap, converge on the next run. The
// database decides liveness: a pack is deleted only when no row needs it.
package gc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// Fault points of a collection, for crash injection in tests.
const (
	// PointAfterDropRow is reached when a superseded checkpoint's row is
	// deleted and its pack is not yet.
	PointAfterDropRow = "after_drop_row"
	// PointMidSweep is reached after the first deletion of the sweep.
	PointMidSweep = "mid_sweep"
	// PointAfterDelete is reached when the sweep deleted its objects and the
	// orphan observations are not yet updated.
	PointAfterDelete = "after_delete"
	// PointAfterPrefix is reached when a removed repository's objects are
	// deleted and its tombstone is not yet.
	PointAfterPrefix = "after_prefix"
)

// Report counts what a collection did, or would do in a dry run.
type Report struct {
	// Boundary is C_gc: the sequence of the newest checkpoint at least gc_grace
	// old, 0 when there is none.
	Boundary uint64
	// CoveredDeleted are the changeset packs covered by the boundary.
	CoveredDeleted int
	// CheckpointsDropped are the checkpoints superseded by the boundary.
	CheckpointsDropped int
	// OrphansDeleted are the unreferenced objects observed at least gc_grace ago.
	OrphansDeleted int
	// OrphansObserved are the unreferenced objects seen for the first time.
	OrphansObserved int
	// OrphansInGrace are the unreferenced objects observed less than gc_grace ago.
	OrphansInGrace int
	// PacksDeleted counts every object deleted (or to delete in a dry run).
	PacksDeleted int
	// BytesReclaimed is the size of those objects.
	BytesReclaimed int64
	// RemovedRepos are the removed repositories whose objects were deleted.
	RemovedRepos int
	// LeaksFound are the repository IDs of the reconciled range that have
	// objects in the bucket and neither a row nor a tombstone.
	LeaksFound int
	// LeaksTombstoned are the tombstones the reconciliation recorded for them.
	LeaksTombstoned int
	// AheadOfAllocator are the repository IDs above the allocator's position
	// that have objects in the bucket (a database restored from an older
	// backup). They are reported and never touched: the allocator will hand
	// them out again.
	AheadOfAllocator int
	// DryRun tells that nothing was deleted or recorded.
	DryRun bool
}

// Collector reclaims packs.
type Collector struct {
	core   *corestore.Core
	logger log.Logger

	// faultHook, when set by a test, is called at the fault points; an error it
	// returns ends the run there, as a crash would. Nil in production.
	faultHook func(point string) error
}

// New returns the collector of core.
func New(core *corestore.Core, logger log.Logger) *Collector {
	return &Collector{core: core, logger: logger}
}

// packPrefix is the bucket prefix of the packs of a repository.
func packPrefix(repoID int64) string {
	return "repos/" + strconv.FormatInt(repoID, 10) + "/packs/"
}

// repoPrefix is the bucket prefix of every object of a repository.
func repoPrefix(repoID int64) string {
	return "repos/" + strconv.FormatInt(repoID, 10) + "/"
}

// CollectCheckpoints drops the checkpoints of one repository that the newest
// checkpoint at least gc_grace old supersedes (C_gc), and nothing else: it is
// step 2 of CollectRepository on its own. No reader uses a checkpoint below
// C_gc (GC BOUNDARY rule), whether or not the changeset packs it covers are
// collected, so the background checkpoints run it with garbage collection off:
// a repository then keeps the checkpoints at or above C_gc, not every
// checkpoint ever built. A crash between a row and its pack leaves an
// unreferenced pack, which the orphan protocol of CollectRepository reclaims.
func (c *Collector) CollectCheckpoints(ctx context.Context, repoID int64, dryRun bool) (Report, error) {
	if err := c.validTimes(); err != nil {
		return Report{}, err
	}
	_, report, _, _, err := c.dropSuperseded(ctx, repoID, dryRun)
	return report, err
}

// dropSuperseded reads the snapshot of the repository, computes the boundary
// C_gc and drops the checkpoints below it (steps 1 and 2 of CollectRepository).
// It returns the snapshot, the report so far, the live checkpoint packs and the
// dropped ones.
func (c *Collector) dropSuperseded(ctx context.Context, repoID int64, dryRun bool) (corestore.GCState, Report, map[string]struct{}, map[string]struct{}, error) {
	// Step 1: one snapshot and the boundary.
	report := Report{DryRun: dryRun}
	st, err := c.core.Meta.GCSnapshot(ctx, repoID)
	if err != nil {
		return st, report, nil, nil, errDatabase(err)
	}
	cutoff := st.Now.Add(-c.core.GCGrace)
	for _, cp := range st.Checkpoints {
		if !cp.CreatedAt.After(cutoff) && cp.Seq > report.Boundary {
			report.Boundary = cp.Seq
		}
	}

	// Step 2: the checkpoints superseded by the boundary, row first. No reader
	// uses a checkpoint below C_gc (GC BOUNDARY rule), and its pack belongs to
	// its row only. A crash between the two leaves an unreferenced pack, which
	// the orphan protocol reclaims.
	live := map[string]struct{}{}
	dropped := map[string]struct{}{}
	for _, cp := range st.Checkpoints {
		if cp.Seq >= report.Boundary {
			live[cp.PackKey] = struct{}{}
			continue
		}
		dropped[cp.PackKey] = struct{}{}
		report.CheckpointsDropped++
		report.PacksDeleted++
		report.BytesReclaimed += cp.PackBytes
		if dryRun {
			continue
		}
		if _, err := c.core.Meta.DeleteCheckpoint(ctx, repoID, cp.Seq); err != nil {
			return st, report, live, dropped, errDatabase(err)
		}
		if err := c.fault(PointAfterDropRow); err != nil {
			return st, report, live, dropped, err
		}
		if err := c.core.Blobs.Delete(ctx, cp.PackKey); err != nil {
			return st, report, live, dropped, errBucket(err)
		}
	}
	return st, report, live, dropped, nil
}

// CollectRepository reclaims the dead packs of one repository. The caller holds
// the repository's maintenance lease, which only prevents wasted runs:
// correctness does not depend on it. The errors carry gRPC codes: NotFound for
// an untracked repository, Unavailable for the database and the bucket.
func (c *Collector) CollectRepository(ctx context.Context, repoID int64, dryRun bool) (Report, error) {
	if err := c.validTimes(); err != nil {
		return Report{}, err
	}

	// Steps 1 and 2: one snapshot, the boundary, the superseded checkpoints.
	st, report, live, dropped, err := c.dropSuperseded(ctx, repoID, dryRun)
	if err != nil {
		return report, err
	}
	cutoff := st.Now.Add(-c.core.GCGrace)

	// Step 3: liveness per key (change G): a key is live when its greatest
	// referencing sequence is above the boundary, covered otherwise.
	covered := map[string]struct{}{}
	for key, seq := range st.Packs {
		if seq > report.Boundary {
			live[key] = struct{}{}
		} else {
			covered[key] = struct{}{}
		}
	}
	for key := range live {
		delete(covered, key)
	}

	// Step 4: the sweep of the bucket prefix.
	var (
		observe []string
		forget  []string
		listed  = map[string]struct{}{}
		deleted bool
	)
	deleteObject := func(info corestore.BlobInfo) error {
		report.PacksDeleted++
		report.BytesReclaimed += info.Size
		if dryRun {
			return nil
		}
		if err := c.core.Blobs.Delete(ctx, info.Key); err != nil {
			return errBucket(err)
		}
		if !deleted {
			deleted = true
			return c.fault(PointMidSweep)
		}
		return nil
	}

	var infos []corestore.BlobInfo
	if err := c.core.Blobs.List(ctx, packPrefix(repoID), func(info corestore.BlobInfo) error {
		infos = append(infos, info)
		return nil
	}); err != nil {
		return report, errBucket(err)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })

	for _, info := range infos {
		key := info.Key
		listed[key] = struct{}{}
		_, observed := st.Orphans[key]

		switch {
		case isKey(live, key):
			if observed {
				// Committed since it was observed (PACK OWNERSHIP RULE 4): keep it.
				forget = append(forget, key)
			}
		case isKey(dropped, key):
			// Deleted in step 2 (or to be, in a dry run); a listing that raced the
			// deletion still shows it.
		case isKey(covered, key):
			if err := deleteObject(info); err != nil {
				return report, err
			}
			report.CoveredDeleted++
			if observed {
				forget = append(forget, key)
			}
		default:
			firstSeen, ok := st.Orphans[key]
			switch {
			case !ok:
				report.OrphansObserved++
				observe = append(observe, key)
			case firstSeen.After(cutoff):
				report.OrphansInGrace++
			default:
				// Observed unreferenced at least gc_grace ago and still unreferenced
				// in this snapshot: no writer can commit it any more (PACK OWNERSHIP
				// RULE 5).
				if err := deleteObject(info); err != nil {
					return report, err
				}
				report.OrphansDeleted++
				forget = append(forget, key)
			}
		}
	}

	// Observations of keys that are gone from the bucket are forgotten too.
	for key := range st.Orphans {
		if _, ok := listed[key]; !ok {
			forget = append(forget, key)
		}
	}

	if dryRun {
		return report, nil
	}
	if err := c.fault(PointAfterDelete); err != nil {
		return report, err
	}
	sort.Strings(observe)
	sort.Strings(forget)
	if err := c.core.Meta.RecordOrphans(ctx, repoID, observe); err != nil {
		return report, errDatabase(err)
	}
	if err := c.core.Meta.ForgetOrphans(ctx, repoID, forget); err != nil {
		return report, errDatabase(err)
	}

	c.logger.WithFields(log.Fields{
		"repository_id":       repoID,
		"boundary":            report.Boundary,
		"covered_deleted":     report.CoveredDeleted,
		"checkpoints_dropped": report.CheckpointsDropped,
		"orphans_deleted":     report.OrphansDeleted,
		"orphans_observed":    report.OrphansObserved,
		"orphans_in_grace":    report.OrphansInGrace,
		"bytes_reclaimed":     report.BytesReclaimed,
	}).InfoContext(ctx, "storage core: garbage collection")
	return report, nil
}

// CollectRemoved reclaims the bucket prefixes of the tombstones of the storage
// (every storage when empty) that are at least gc_grace old, then deletes the
// tombstones (change F). A writer whose record read found a removed repository
// did so before removed_at and uploads only until t_r + W, so no upload arrives
// in the prefix after the sweep. A crash after a partial deletion leaves the
// tombstone; the next run finishes it.
func (c *Collector) CollectRemoved(ctx context.Context, storage string, dryRun bool) (Report, error) {
	if err := c.validTimes(); err != nil {
		return Report{}, err
	}

	removed, err := c.core.Meta.ListRemovedRepositories(ctx, storage, c.core.GCGrace)
	if err != nil {
		return Report{}, errDatabase(err)
	}

	report := Report{DryRun: dryRun}
	for _, r := range removed {
		var infos []corestore.BlobInfo
		if err := c.core.Blobs.List(ctx, repoPrefix(r.RepoID), func(info corestore.BlobInfo) error {
			infos = append(infos, info)
			return nil
		}); err != nil {
			return report, errBucket(err)
		}

		for _, info := range infos {
			report.PacksDeleted++
			report.BytesReclaimed += info.Size
			if dryRun {
				continue
			}
			if err := c.core.Blobs.Delete(ctx, info.Key); err != nil {
				return report, errBucket(err)
			}
		}
		report.RemovedRepos++
		if dryRun {
			continue
		}
		if err := c.fault(PointAfterPrefix); err != nil {
			return report, err
		}
		if err := c.core.Meta.DeleteRemovedRepository(ctx, r.RepoID); err != nil {
			return report, errDatabase(err)
		}
		c.logger.WithFields(log.Fields{
			"repository_id": r.RepoID,
			"repository":    r.Key.String(),
			"objects":       len(infos),
		}).InfoContext(ctx, "storage core: the objects of a removed repository are deleted")
	}
	return report, nil
}

const (
	// reconcileBatch bounds the repository IDs one reconciliation examines.
	reconcileBatch = 256
	// aheadProbe is the number of IDs above the allocator's position whose
	// prefixes a reconciliation checks.
	aheadProbe = 16
)

// errFound stops a listing at its first object.
var errFound = errors.New("found")

// hasObjects reports whether the bucket has an object under the repository's
// prefix. It reads one object at most (one page of a paged listing).
func (c *Collector) hasObjects(ctx context.Context, repoID int64) (bool, error) {
	err := c.core.Blobs.List(ctx, repoPrefix(repoID), func(corestore.BlobInfo) error { return errFound })
	switch {
	case errors.Is(err, errFound):
		return true, nil
	case err != nil:
		return false, errBucket(err)
	}
	return false, nil
}

// Reconcile records a tombstone for every repository ID whose row was deleted
// without one (before the trigger graft_repositories_tombstone existed) and
// whose objects are still in the bucket; CollectRemoved then deletes them
// like those of any removed repository (doc/graft/PLAN7.md, "Removed
// repositories", reconciliation). It examines the IDs after its cursor, at
// most reconcileBatch per run, and only those the allocator handed out at
// least the write deadline W ago on the database clock: a creation still in
// flight is never examined (on Postgres an allocated ID precedes its row). An
// ID without a row or a tombstone gets one only when its prefix has objects;
// the cursor then moves past the batch, so a run that stopped is resumed by
// the next one and nothing is examined twice. IDs above the allocator that
// have objects are counted and logged, never touched. A dry run records
// nothing.
func (c *Collector) Reconcile(ctx context.Context, dryRun bool) (Report, error) {
	if err := c.validTimes(); err != nil {
		return Report{}, err
	}

	view, err := c.core.Meta.ObserveAllocator(ctx, c.core.WriteDeadline(), dryRun)
	if err != nil {
		return Report{}, errDatabase(err)
	}
	report := Report{DryRun: dryRun}

	for id := view.Position + 1; id <= view.Position+aheadProbe; id++ {
		found, err := c.hasObjects(ctx, id)
		if err != nil {
			return report, err
		}
		if found {
			report.AheadOfAllocator++
			c.logger.WithFields(log.Fields{
				"repository_id":      id,
				"allocator_position": view.Position,
			}).WarnContext(ctx, "storage core: the bucket has objects of a repository ID the database has not allocated; was the database restored from an older backup? They are left alone")
		}
	}

	upTo := min(view.Bound, view.DoneThrough+reconcileBatch)
	if upTo <= view.DoneThrough {
		return report, nil
	}
	ids, err := c.core.Meta.UnrecordedIDs(ctx, view.DoneThrough, upTo)
	if err != nil {
		return report, errDatabase(err)
	}
	for _, id := range ids {
		found, err := c.hasObjects(ctx, id)
		if err != nil {
			return report, err
		}
		if !found {
			continue
		}
		report.LeaksFound++
		if dryRun {
			continue
		}
		recorded, err := c.core.Meta.RecordLeak(ctx, id)
		if err != nil {
			return report, errDatabase(err)
		}
		if recorded {
			report.LeaksTombstoned++
			c.logger.WithField("repository_id", id).InfoContext(ctx,
				"storage core: the bucket has objects of a repository whose row was deleted without a tombstone; they are collected after gc_grace")
		}
	}
	if dryRun {
		return report, nil
	}
	if err := c.core.Meta.AdvanceReconcile(ctx, upTo); err != nil {
		return report, errDatabase(err)
	}
	return report, nil
}

// validTimes refuses to collect with times that would break the proofs.
func (c *Collector) validTimes() error {
	if c.core.GCGrace <= 0 || c.core.GCMargin <= 0 || c.core.WriteDeadline() <= 0 {
		return structerr.NewFailedPrecondition("storage core: invalid garbage collection times: gc_grace %s, gc_margin %s",
			c.core.GCGrace, c.core.GCMargin)
	}
	return nil
}

func (c *Collector) fault(point string) error {
	if c.faultHook == nil {
		return nil
	}
	if err := c.faultHook(point); err != nil {
		return fmt.Errorf("fault at %s: %w", point, err)
	}
	return nil
}

func isKey(set map[string]struct{}, key string) bool {
	_, ok := set[key]
	return ok
}

// errDatabase maps a database error of a collection.
func errDatabase(err error) error {
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return structerr.NewNotFound("the repository is not tracked by the storage core")
	}
	return structerr.NewUnavailable("storage core: %w", err)
}

// errBucket maps a bucket error of a collection.
func errBucket(err error) error {
	return structerr.NewUnavailable("storage core: bucket: %w", err)
}
