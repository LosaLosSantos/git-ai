package gc_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gc"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

const grace = time.Hour

func forEachDialect(t *testing.T, test func(t *testing.T, e *env)) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			test(t, newEnv(t, dialect))
		})
	}
}

// env is a storage core with one repository, and the helpers to put rows and
// objects in it and to age them on the database clock.
type env struct {
	t      *testing.T
	ctx    context.Context
	core   *corestore.Core
	store  *sqlstore.Store
	gc     *gc.Collector
	repo   corestore.Repository
	nextID int
}

func newEnv(t *testing.T, dialect sqlstore.Dialect) *env {
	t.Helper()

	core := coretest.Open(t, coretest.Options{Dialect: dialect, GCGrace: grace, GCMargin: 5 * time.Minute})
	ctx := testhelper.Context(t)
	e := &env{t: t, ctx: ctx, core: core, store: core.Meta.(*sqlstore.Store), gc: gc.New(core, testhelper.NewLogger(t))}
	e.repo = e.register("repo")
	return e
}

func (e *env) register(name string) corestore.Repository {
	e.t.Helper()
	repo, _, err := e.core.Meta.RegisterRepository(e.ctx, corestore.RepoKey{Storage: "default", RelativePath: name + ".git"}, "sha1", corestore.InitialState{})
	require.NoError(e.t, err)
	return repo
}

// upload stores a new pack object with unique content under a new owned key
// of the repository, referenced by no row.
func (e *env) upload(repoID int64) string {
	e.t.Helper()
	e.nextID++
	return e.uploadContent(repoID, []byte(fmt.Sprintf("pack %d of %d", e.nextID, repoID)))
}

func (e *env) uploadContent(repoID int64, content []byte) string {
	e.t.Helper()
	key := corestore.NewPackKey(repoID, sha256.Sum256(content))
	require.NoError(e.t, e.core.Blobs.Put(e.ctx, key, bytes.NewReader(content)))
	return key
}

// changeset commits the next changeset of the repository, moving main, with
// a new pack; it returns the pack key.
func (e *env) changeset() string {
	e.t.Helper()
	key := e.upload(e.repo.ID)
	e.commit(key)
	return key
}

// commit commits the next changeset with the given pack key.
func (e *env) commit(key string) uint64 {
	e.t.Helper()
	res, err := e.core.Meta.CommitChangeset(e.ctx, corestore.ChangesetInput{
		RepoID:    e.repo.ID,
		Updates:   []corestore.RefUpdate{{Name: "refs/heads/main", Expect: corestore.ExpectAny, New: corestore.Target(fmt.Sprintf("%040x", e.nextID+1000))}},
		Operation: "test",
		Actor:     "tester",
		NodeID:    "node-a",
		PackKey:   key, PackDeadline: time.Now().Add(time.Hour),
		PackBytes: 1,
	})
	require.NoError(e.t, err)
	return res.Seq
}

// checkpoint records a checkpoint at seq with a new pack; it returns the key.
func (e *env) checkpoint(seq uint64) string {
	e.t.Helper()
	key := e.upload(e.repo.ID)
	_, created, err := e.core.Meta.CreateCheckpoint(e.ctx, e.repo.ID, corestore.CheckpointInput{
		Seq:     seq,
		Refs:    map[string]corestore.Target{"refs/heads/main": corestore.Target(strings.Repeat("a", 40))},
		PackKey: key, PackDeadline: time.Now().Add(time.Hour),
		PackBytes: 1,
	})
	require.NoError(e.t, err)
	require.True(e.t, created)
	return key
}

// exec runs a statement with ? placeholders on the database.
func (e *env) exec(query string, args ...any) {
	e.t.Helper()
	if e.store.Dialect() == sqlstore.Postgres {
		var b strings.Builder
		n := 0
		for _, r := range query {
			if r == '?' {
				n++
				fmt.Fprintf(&b, "$%d", n)
				continue
			}
			b.WriteRune(r)
		}
		query = b.String()
	}
	_, err := e.store.DB().ExecContext(e.ctx, query, args...)
	require.NoError(e.t, err)
}

// past returns a database time value d before now.
func (e *env) past(d time.Duration) any {
	t := time.Now().Add(-d).UTC()
	if e.store.Dialect() == sqlstore.SQLite {
		return t.Format(time.RFC3339Nano)
	}
	return t
}

func (e *env) ageCheckpoint(seq uint64, d time.Duration) {
	e.exec("UPDATE graft_checkpoints SET created_at = ? WHERE repo_id = ? AND seq = ?", e.past(d), e.repo.ID, int64(seq))
}

func (e *env) ageObservations(d time.Duration) {
	e.exec("UPDATE graft_gc_orphans SET first_seen = ? WHERE repo_id = ?", e.past(d), e.repo.ID)
}

// objects lists the objects of the repository in the bucket.
func (e *env) objects(repoID int64) []string {
	e.t.Helper()
	var keys []string
	require.NoError(e.t, e.core.Blobs.List(e.ctx, fmt.Sprintf("repos/%d/", repoID), func(info corestore.BlobInfo) error {
		keys = append(keys, info.Key)
		return nil
	}))
	sort.Strings(keys)
	return keys
}

func (e *env) observations() map[string]time.Time {
	e.t.Helper()
	st, err := e.core.Meta.GCSnapshot(e.ctx, e.repo.ID)
	require.NoError(e.t, err)
	return st.Orphans
}

func (e *env) collect(dryRun bool) gc.Report {
	e.t.Helper()
	report, err := e.gc.CollectRepository(e.ctx, e.repo.ID, dryRun)
	require.NoError(e.t, err)
	return report
}

func sorted(keys ...string) []string {
	sort.Strings(keys)
	return keys
}

// TestCollectOrphans is the orphan protocol: an unreferenced object is
// observed, kept while its observation is younger than gc_grace, and deleted
// after; a re-run is a no-op.
func TestCollectOrphans(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		p1 := e.changeset()
		orphan := e.upload(e.repo.ID)

		report := e.collect(false)
		require.Equal(t, gc.Report{OrphansObserved: 1}, report)
		require.Contains(t, e.observations(), orphan)

		report = e.collect(false)
		require.Equal(t, gc.Report{OrphansInGrace: 1}, report)
		require.Equal(t, sorted(p1, orphan), e.objects(e.repo.ID))

		e.ageObservations(grace + time.Minute)
		report = e.collect(false)
		require.Equal(t, 1, report.OrphansDeleted)
		require.Equal(t, 1, report.PacksDeleted)
		require.Equal(t, []string{p1}, e.objects(e.repo.ID))
		require.Empty(t, e.observations())

		require.Equal(t, gc.Report{}, e.collect(false), "a re-run is a no-op")
	})
}

// TestCollectKeepsAPackCommittedAfterItsObservation is crash/race case 3: an
// object observed unreferenced (an upload whose commit was still running) and
// committed by its writer before its deadline is kept, and its observation is
// forgotten.
func TestCollectKeepsAPackCommittedAfterItsObservation(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		late := e.upload(e.repo.ID)
		require.Equal(t, 1, e.collect(false).OrphansObserved)

		e.commit(late)
		e.ageObservations(grace + time.Minute)
		report := e.collect(false)
		require.Zero(t, report.OrphansDeleted)
		require.Equal(t, []string{late}, e.objects(e.repo.ID))
		require.Empty(t, e.observations(), "the observation of a referenced key is forgotten")
	})
}

// TestCollectForgetsObservationsOfGoneObjects: an observed object that left the
// bucket (deleted by an overlapping run) loses its observation.
func TestCollectForgetsObservationsOfGoneObjects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		gone := e.upload(e.repo.ID)
		require.Equal(t, 1, e.collect(false).OrphansObserved)
		require.NoError(t, e.core.Blobs.Delete(e.ctx, gone))

		require.Equal(t, gc.Report{}, e.collect(false))
		require.Empty(t, e.observations())
	})
}

// TestCollectBoundary covers the GC BOUNDARY rule: only checkpoints at least
// gc_grace old count; packs of changesets up to the boundary are covered, the
// checkpoints below it are dropped, everything above it is kept.
func TestCollectBoundary(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		var packs []string
		for range 5 {
			packs = append(packs, e.changeset())
		}
		cp2 := e.checkpoint(2)
		cp4 := e.checkpoint(4)

		// No checkpoint is old enough: nothing is covered.
		require.Equal(t, gc.Report{}, e.collect(false))

		// cp2 is old enough: changesets 1 and 2 are covered.
		e.ageCheckpoint(2, grace+time.Minute)
		report := e.collect(false)
		require.Equal(t, uint64(2), report.Boundary)
		require.Equal(t, 2, report.CoveredDeleted)
		require.Zero(t, report.CheckpointsDropped)
		require.Equal(t, sorted(packs[2], packs[3], packs[4], cp2, cp4), e.objects(e.repo.ID))

		// cp4 is old enough too: cp2 is dropped, changesets 3 and 4 covered.
		e.ageCheckpoint(4, grace+time.Minute)
		report = e.collect(false)
		require.Equal(t, uint64(4), report.Boundary)
		require.Equal(t, 2, report.CoveredDeleted)
		require.Equal(t, 1, report.CheckpointsDropped)
		require.Equal(t, 3, report.PacksDeleted)
		require.Equal(t, sorted(packs[4], cp4), e.objects(e.repo.ID))

		newest, ok, err := e.core.Meta.NewestCheckpoint(e.ctx, e.repo.ID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, uint64(4), newest.Seq)
		_, err = e.core.Meta.CheckpointRefs(e.ctx, e.repo.ID, 2)
		require.ErrorIs(t, err, corestore.ErrCheckpointNotFound)

		// History is never deleted: every changeset row stays.
		changesets, err := e.core.Meta.ListChangesets(e.ctx, e.repo.ID, 0, 5)
		require.NoError(t, err)
		require.Len(t, changesets, 5)

		require.Equal(t, gc.Report{Boundary: 4}, e.collect(false), "a re-run is a no-op")
	})
}

// TestCollectIdenticalPacks is crash/race case 1: two writes with
// byte-identical packs own one object each, so covering the first deletes only
// its own object and the second write's pack stays.
func TestCollectIdenticalPacks(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		content := []byte("the same pack twice")
		first := e.uploadContent(e.repo.ID, content)
		e.commit(first)
		second := e.uploadContent(e.repo.ID, content)
		require.NotEqual(t, first, second)
		e.checkpoint(1)
		secondSeq := e.commit(second)
		require.Equal(t, uint64(2), secondSeq)

		e.ageCheckpoint(1, grace+time.Minute)
		report := e.collect(false)
		require.Equal(t, 1, report.CoveredDeleted)
		require.Contains(t, e.objects(e.repo.ID), second)
		require.NotContains(t, e.objects(e.repo.ID), first)
	})
}

// TestCollectDryRun deletes and records nothing and reports what a real run
// does.
func TestCollectDryRun(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		for range 3 {
			e.changeset()
		}
		e.checkpoint(1)
		e.checkpoint(2)
		orphan := e.upload(e.repo.ID)
		require.Equal(t, 1, e.collect(false).OrphansObserved)
		e.ageObservations(grace + time.Minute)
		e.ageCheckpoint(1, grace+time.Minute)
		e.ageCheckpoint(2, grace+time.Minute)
		newOrphan := e.upload(e.repo.ID)

		before := e.objects(e.repo.ID)
		observations := e.observations()
		dry := e.collect(true)
		require.True(t, dry.DryRun)
		require.Equal(t, before, e.objects(e.repo.ID))
		require.Equal(t, observations, e.observations())

		real := e.collect(false)
		dry.DryRun = false
		require.Equal(t, real, dry)
		require.Equal(t, gc.Report{Boundary: 2, CoveredDeleted: 2, CheckpointsDropped: 1, OrphansDeleted: 1, OrphansObserved: 1, PacksDeleted: 4, BytesReclaimed: real.BytesReclaimed}, real)
		require.NotContains(t, e.objects(e.repo.ID), orphan)
		require.Contains(t, e.objects(e.repo.ID), newOrphan)
	})
}

// TestCollectCrashPoints: a run that dies at any fault point leaves a state
// that the next runs converge from.
func TestCollectCrashPoints(t *testing.T) {
	errCrash := errors.New("crash")

	for _, point := range []string{gc.PointAfterDropRow, gc.PointMidSweep, gc.PointAfterDelete} {
		t.Run(point, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, e *env) {
				var packs []string
				for range 4 {
					packs = append(packs, e.changeset())
				}
				e.checkpoint(1)
				cp3 := e.checkpoint(3)
				orphan := e.upload(e.repo.ID)
				require.Equal(t, 1, e.collect(false).OrphansObserved)
				e.ageObservations(grace + time.Minute)
				e.ageCheckpoint(1, grace+time.Minute)
				e.ageCheckpoint(3, grace+time.Minute)

				gc.SetFaultHook(e.gc, func(p string) error {
					if p == point {
						return errCrash
					}
					return nil
				})
				_, err := e.gc.CollectRepository(e.ctx, e.repo.ID, false)
				require.ErrorIs(t, err, errCrash)
				gc.SetFaultHook(e.gc, nil)

				// The next run finishes; a checkpoint pack whose row was dropped before
				// the crash is an orphan the protocol reclaims after gc_grace.
				e.collect(false)
				e.ageObservations(grace + time.Minute)
				e.collect(false)
				require.Equal(t, sorted(packs[3], cp3), e.objects(e.repo.ID))
				require.NotContains(t, e.objects(e.repo.ID), orphan)
				require.Empty(t, e.observations())
				require.Equal(t, gc.Report{Boundary: 3}, e.collect(false))
			})
		})
	}
}

// TestCollectConcurrently is crash/race case 9 for two collectors: overlapping
// runs make only safe decisions and converge.
func TestCollectConcurrently(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		var packs []string
		for range 6 {
			packs = append(packs, e.changeset())
		}
		cp4 := e.checkpoint(4)
		for range 5 {
			e.upload(e.repo.ID)
		}
		e.collect(false)
		e.ageObservations(grace + time.Minute)
		e.ageCheckpoint(4, grace+time.Minute)

		other := gc.New(e.core, testhelper.NewLogger(t))
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, c := range []*gc.Collector{e.gc, other} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = c.CollectRepository(e.ctx, e.repo.ID, false)
			}()
		}
		wg.Wait()
		require.NoError(t, errors.Join(errs...))

		require.Equal(t, sorted(packs[4], packs[5], cp4), e.objects(e.repo.ID))
		require.Empty(t, e.observations())
	})
}

func TestCollectUntrackedRepository(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		_, err := e.gc.CollectRepository(e.ctx, e.repo.ID+1000, false)
		require.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})
}

// TestCollectRemoved is change F: the objects of a removed repository are
// deleted once its tombstone is gc_grace old, then the tombstone; a crash after
// the objects, or a partial deletion, converges on the next run.
func TestCollectRemoved(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		removed := e.register("removed")
		var keys []string
		for range 4 {
			keys = append(keys, e.upload(removed.ID))
		}
		kept := e.changeset()
		ok, err := e.core.Meta.DeleteRepository(e.ctx, removed.Key)
		require.NoError(t, err)
		require.True(t, ok)

		report, err := e.gc.CollectRemoved(e.ctx, "", false)
		require.NoError(t, err)
		require.Equal(t, gc.Report{}, report, "the tombstone is younger than gc_grace")
		require.Len(t, e.objects(removed.ID), 4)

		e.exec("UPDATE graft_gc_removed SET removed_at = ? WHERE repo_id = ?", e.past(grace+time.Minute), removed.ID)

		dry, err := e.gc.CollectRemoved(e.ctx, "", true)
		require.NoError(t, err)
		require.Equal(t, gc.Report{RemovedRepos: 1, PacksDeleted: 4, BytesReclaimed: dry.BytesReclaimed, DryRun: true}, dry)
		require.Len(t, e.objects(removed.ID), 4)

		// A partial deletion, then a crash after the prefix.
		require.NoError(t, e.core.Blobs.Delete(e.ctx, keys[0]))
		gc.SetFaultHook(e.gc, func(p string) error {
			if p == gc.PointAfterPrefix {
				return errors.New("crash")
			}
			return nil
		})
		_, err = e.gc.CollectRemoved(e.ctx, "", false)
		require.Error(t, err)
		gc.SetFaultHook(e.gc, nil)
		require.Empty(t, e.objects(removed.ID))
		tombstones, err := e.core.Meta.ListRemovedRepositories(e.ctx, "", 0)
		require.NoError(t, err)
		require.Len(t, tombstones, 1, "the tombstone stays until its prefix is gone")

		report, err = e.gc.CollectRemoved(e.ctx, "", false)
		require.NoError(t, err)
		require.Equal(t, 1, report.RemovedRepos)
		tombstones, err = e.core.Meta.ListRemovedRepositories(e.ctx, "", 0)
		require.NoError(t, err)
		require.Empty(t, tombstones)

		require.Equal(t, []string{kept}, e.objects(e.repo.ID), "other repositories are untouched")
	})
}

// tombstones returns the tombstones of every storage, whatever their age.
func (e *env) tombstones() map[int64]corestore.RemovedRepository {
	e.t.Helper()
	removed, err := e.core.Meta.ListRemovedRepositories(e.ctx, "", 0)
	require.NoError(e.t, err)
	out := map[int64]corestore.RemovedRepository{}
	for _, r := range removed {
		out[r.RepoID] = r
	}
	return out
}

// leak registers a repository, stores objects under its prefix and deletes
// its row and its tombstone, as a row deleted by hand before the trigger
// existed left them. It returns the repository ID.
func (e *env) leak(name string, objects int) int64 {
	e.t.Helper()
	repo := e.register(name)
	for range objects {
		e.upload(repo.ID)
	}
	e.exec("DELETE FROM graft_repositories WHERE repo_id = ?", repo.ID)
	e.exec("DELETE FROM graft_gc_removed WHERE repo_id = ?", repo.ID)
	return repo.ID
}

// allocator returns the allocator view without recording an observation.
func (e *env) allocator() corestore.AllocatorView {
	e.t.Helper()
	view, err := e.core.Meta.ObserveAllocator(e.ctx, e.core.WriteDeadline(), true)
	require.NoError(e.t, err)
	return view
}

// ageAllocator replaces the observations of the allocator by one of its
// current position made d ago.
func (e *env) ageAllocator(d time.Duration) {
	e.t.Helper()
	position := e.allocator().Position
	e.exec("DELETE FROM graft_gc_allocator_marks")
	e.exec("INSERT INTO graft_gc_allocator_marks (observed_at, position) VALUES (?, ?)", e.past(d), position)
}

func (e *env) reconcile(dryRun bool) gc.Report {
	e.t.Helper()
	report, err := e.gc.Reconcile(e.ctx, dryRun)
	require.NoError(e.t, err)
	return report
}

// TestDeletedRowGetsATombstone: a repository row deleted by hand (the repair
// of STORAGE.md, "State and verify") gets its tombstone from the trigger, on
// the database clock, and its objects are collected like those of a removed
// repository (integrated review m4).
func TestDeletedRowGetsATombstone(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		repo := e.register("by-hand")
		e.upload(repo.ID)
		e.exec("DELETE FROM graft_repositories WHERE repo_id = ?", repo.ID)

		tombstone, ok := e.tombstones()[repo.ID]
		require.True(t, ok, "the trigger records the tombstone")
		require.Equal(t, repo.Key, tombstone.Key)
		require.WithinDuration(t, time.Now(), tombstone.RemovedAt, time.Minute)

		e.exec("UPDATE graft_gc_removed SET removed_at = ? WHERE repo_id = ?", e.past(grace+time.Minute), repo.ID)
		report, err := e.gc.CollectRemoved(e.ctx, "default", false)
		require.NoError(t, err)
		require.Equal(t, 1, report.RemovedRepos)
		require.Empty(t, e.objects(repo.ID))
	})
}

// TestReconcileTombstonesLeaks: the objects of a row deleted without a
// tombstone get one, only for IDs allocated at least W ago, only when the
// prefix has objects, once; the tombstone has no key and is collected by
// every storage's collection after gc_grace (integrated review m4).
func TestReconcileTombstonesLeaks(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		live := e.changeset()
		leaked := e.leak("leaked", 2)
		empty := e.leak("empty", 0)

		require.Equal(t, gc.Report{}, e.reconcile(false), "no allocation is W old yet")
		require.Len(t, e.objects(leaked), 2)

		e.ageAllocator(grace)
		require.Equal(t, gc.Report{LeaksFound: 1, DryRun: true}, e.reconcile(true))
		require.NotContains(t, e.tombstones(), leaked, "a dry run records nothing")

		require.Equal(t, gc.Report{LeaksFound: 1, LeaksTombstoned: 1}, e.reconcile(false))
		tombstones := e.tombstones()
		require.Contains(t, tombstones, leaked)
		require.Equal(t, corestore.RepoKey{}, tombstones[leaked].Key, "the key went with the row")
		require.NotContains(t, tombstones, empty, "an ID without objects needs nothing")
		require.Equal(t, e.allocator().Position, e.allocator().DoneThrough, "the cursor is past the reconciled IDs")

		require.Equal(t, gc.Report{}, e.reconcile(false), "a second run examines nothing again")

		report, err := e.gc.CollectRemoved(e.ctx, "default", false)
		require.NoError(t, err)
		require.Zero(t, report.RemovedRepos, "the tombstone is younger than gc_grace")
		e.exec("UPDATE graft_gc_removed SET removed_at = ? WHERE repo_id = ?", e.past(grace+time.Minute), leaked)
		report, err = e.gc.CollectRemoved(e.ctx, "default", false)
		require.NoError(t, err)
		require.Equal(t, 1, report.RemovedRepos, "a storage's collection takes the tombstones without a key")
		require.Empty(t, e.objects(leaked))
		require.Equal(t, []string{live}, e.objects(e.repo.ID), "the live repository is untouched")
	})
}

// TestReconcileIsPagedAndResumable: a run examines at most one batch of IDs
// after its cursor; the next run goes on from there, and IDs above the
// allocator with objects are reported and never touched.
func TestReconcileIsPagedAndResumable(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *env) {
		const position = 2*gc.ReconcileBatch + 40
		if e.store.Dialect() == sqlstore.Postgres {
			e.exec(fmt.Sprintf("SELECT setval('graft_repositories_repo_id_seq', %d)", position))
		} else {
			e.exec("UPDATE sqlite_sequence SET seq = ? WHERE name = 'graft_repositories'", position)
		}
		first, second := int64(5), int64(gc.ReconcileBatch+10)
		e.upload(first)
		e.upload(second)
		ahead := e.upload(position + 3)
		e.ageAllocator(grace)

		require.Equal(t, gc.Report{LeaksFound: 1, LeaksTombstoned: 1, AheadOfAllocator: 1}, e.reconcile(false))
		require.Contains(t, e.tombstones(), first)
		require.NotContains(t, e.tombstones(), second, "beyond the first batch")
		require.EqualValues(t, gc.ReconcileBatch, e.allocator().DoneThrough)

		// A run that stopped after recording a tombstone, before its cursor
		// moved: the next one does not count it again.
		_, err := e.core.Meta.RecordLeak(e.ctx, second)
		require.NoError(t, err)
		require.Equal(t, gc.Report{AheadOfAllocator: 1}, e.reconcile(false))
		require.EqualValues(t, 2*gc.ReconcileBatch, e.allocator().DoneThrough)
		require.Equal(t, gc.Report{AheadOfAllocator: 1}, e.reconcile(false))
		require.EqualValues(t, position, e.allocator().DoneThrough)

		require.NotContains(t, e.tombstones(), int64(position+3))
		require.Equal(t, []string{ahead}, e.objects(position+3), "never touched")
	})
}

// TestReconcileNeverCollectsACreationInFlight holds a creation between the
// allocation of its ID and the commit of its row across reconciliations, on
// Postgres, whose allocator counts the ID before the row commits. (SQLite's
// allocator is transactional: an uncommitted creation's ID is not allocated
// for any other reader, and the single writer lock serialises the creation
// with the reconciliation's writes.) An object written under the ID before
// the commit, which no writer of the storage core does, is the worst case.
// The ID is not examined while its allocation is younger than W; examined
// later it may get a tombstone, but once the row commits the tombstone is not
// a removal: it is never collected, and a later deletion of the row starts a
// fresh grace.
func TestReconcileNeverCollectsACreationInFlight(t *testing.T) {
	if !slices.Contains(coretest.Dialects(), sqlstore.Postgres) {
		t.Skip("the allocator counts uncommitted creations on Postgres only, which is not configured")
	}
	e := newEnv(t, sqlstore.Postgres)

	tx, err := e.store.DB().BeginTx(e.ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var id int64
	require.NoError(t, tx.QueryRowContext(e.ctx, `
		INSERT INTO graft_repositories (storage, relative_path, object_format, head_seq, log_seq, next_review_id, created_at)
		VALUES ('default', 'in-flight.git', 'sha1', 0, 0, 1, now()) RETURNING repo_id`).Scan(&id))
	key := e.upload(id)

	require.Equal(t, e.allocator().Position, id, "the allocator counts the creation")
	require.Equal(t, gc.Report{}, e.reconcile(false), "an allocation younger than W is not examined")
	require.NotContains(t, e.tombstones(), id)

	// A creation that outlives W: its ID is examined and gets a tombstone.
	e.ageAllocator(grace)
	require.Equal(t, gc.Report{LeaksFound: 1, LeaksTombstoned: 1}, e.reconcile(false))
	require.NoError(t, tx.Commit())

	e.exec("UPDATE graft_gc_removed SET removed_at = ? WHERE repo_id = ?", e.past(grace+time.Minute), id)
	report, err := e.gc.CollectRemoved(e.ctx, "default", false)
	require.NoError(t, err)
	require.Zero(t, report.RemovedRepos, "a tombstone whose ID has a row is not a removal")
	require.Equal(t, []string{key}, e.objects(id))

	// Its deletion is a removal, with a grace from the deletion.
	e.exec("DELETE FROM graft_repositories WHERE repo_id = ?", id)
	tombstone := e.tombstones()[id]
	require.Equal(t, "in-flight.git", tombstone.Key.RelativePath)
	require.WithinDuration(t, time.Now(), tombstone.RemovedAt, time.Minute)
	report, err = e.gc.CollectRemoved(e.ctx, "default", false)
	require.NoError(t, err)
	require.Zero(t, report.RemovedRepos)
	require.Equal(t, []string{key}, e.objects(id))
}
