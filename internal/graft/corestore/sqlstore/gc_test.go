package sqlstore_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// ownedKey returns a new owned pack key of the repository for content.
func ownedKey(repoID int64, content string) string {
	return corestore.NewPackKey(repoID, sha256.Sum256([]byte(content)))
}

// legacyKey returns a key in the format of v0.1.0, which shared pack objects.
func legacyKey(repoID int64, content string) string {
	return fmt.Sprintf("repos/%d/packs/%x.pack", repoID, sha256.Sum256([]byte(content)))
}

// commitPack commits one reference update with a pack key.
func commitPack(ctx context.Context, s *sqlstore.Store, repoID int64, u corestore.RefUpdate, packKey string) (corestore.CommitResult, error) {
	return s.CommitChangeset(ctx, corestore.ChangesetInput{
		RepoID:    repoID,
		Updates:   []corestore.RefUpdate{u},
		Operation: "test",
		Actor:     "tester",
		NodeID:    "node-a",
		PackKey:   packKey, PackDeadline: time.Now().Add(time.Hour),
		PackBytes: 1,
	})
}

// execSQL runs a statement written with "?" placeholders directly on the
// database of the store, for tests that set up rows the API cannot (an old
// time, a row of an earlier version).
func execSQL(t *testing.T, ctx context.Context, s *sqlstore.Store, query string, args ...any) {
	t.Helper()

	if s.Dialect() == sqlstore.Postgres {
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

	_, err := s.DB().ExecContext(ctx, query, args...)
	require.NoError(t, err)
}

// timeValue returns t as the dialect stores times.
func timeValue(s *sqlstore.Store, t time.Time) any {
	if s.Dialect() == sqlstore.SQLite {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return t.UTC()
}

// headRepo registers a repository and commits n changesets on refs/heads/main,
// each with its own owned pack key, which it returns in sequence order.
func headRepo(t *testing.T, ctx context.Context, s *sqlstore.Store, name string, n int) (corestore.Repository, []string) {
	t.Helper()

	repo := register(t, ctx, s, testKey(name))
	keys := make([]string, n)
	prev := corestore.Target("")
	for i := range n {
		keys[i] = ownedKey(repo.ID, fmt.Sprintf("%s pack %d", name, i))
		next := oid(i + 1)
		_, err := commitPack(ctx, s, repo.ID, update("refs/heads/main", prev, next), keys[i])
		require.NoError(t, err)
		prev = next
	}
	return repo, keys
}

func TestOwnedPackKeyTrigger(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("trigger"))

		for _, key := range []string{legacyKey(repo.ID, "shared"), "not-a-pack-key"} {
			_, err := commitPack(ctx, s, repo.ID, create("refs/heads/main", oid(1)), key)
			require.ErrorContains(t, err, sqlstore.NotOwnedPackKeyMessage, key)
		}
		// Nothing of the refused changesets was written.
		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{})

		res, err := commitPack(ctx, s, repo.ID, create("refs/heads/main", oid(1)), ownedKey(repo.ID, "owned"))
		require.NoError(t, err)
		require.Equal(t, uint64(1), res.Seq)

		// A changeset without new objects has no pack key.
		res, err = commit(ctx, s, repo.ID, update("refs/heads/main", oid(1), oid(2)))
		require.NoError(t, err)
		require.Equal(t, uint64(2), res.Seq)
	})
}

func TestV010DatabaseMigrates(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			ctx := testhelper.Context(t)
			dsn := coretest.DatabaseURL(t, dialect)

			// A database as v0.1.0 left it: one pack object shared by changesets 1
			// and 3 (content-addressed dedup), and no owned-key trigger yet.
			old, err := sqlstore.OpenV010(ctx, dsn)
			require.NoError(t, err)
			repo, _, err := old.RegisterRepository(ctx, testKey("v010"), "sha1", corestore.InitialState{})
			require.NoError(t, err)
			shared, other := legacyKey(repo.ID, "shared"), legacyKey(repo.ID, "other")
			for i, key := range []string{shared, other, shared} {
				_, err := commitPack(ctx, old, repo.ID, force("refs/heads/main", oid(i+1)), key)
				require.NoError(t, err)
			}
			require.NoError(t, old.Close())

			// The current binary applies migration 2 at Open.
			s := openStore(t, ctx, dsn)

			changesets, err := s.ListChangesets(ctx, repo.ID, 0, 10)
			require.NoError(t, err)
			require.Len(t, changesets, 3)
			require.Equal(t, []string{shared, other, shared},
				[]string{changesets[0].PackKey, changesets[1].PackKey, changesets[2].PackKey}, "legacy rows stay readable")

			st, err := s.GCSnapshot(ctx, repo.ID)
			require.NoError(t, err)
			require.Equal(t, map[string]uint64{shared: 3, other: 2}, st.Packs, "a shared key is live up to its greatest sequence")
			require.Empty(t, st.Checkpoints)
			require.Empty(t, st.Orphans)

			_, err = commitPack(ctx, s, repo.ID, force("refs/heads/main", oid(9)), legacyKey(repo.ID, "new"))
			require.ErrorContains(t, err, sqlstore.NotOwnedPackKeyMessage)
			_, err = commitPack(ctx, s, repo.ID, force("refs/heads/main", oid(9)), ownedKey(repo.ID, "new"))
			require.NoError(t, err)
		})
	}
}

func TestCheckpointLifecycle(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo, _ := headRepo(t, ctx, s, "checkpoint", 3)

		_, ok, err := s.NewestCheckpoint(ctx, repo.ID)
		require.NoError(t, err)
		require.False(t, ok)

		refs := map[string]corestore.Target{
			"HEAD":                 corestore.SymbolicTarget("refs/heads/main"),
			"refs/heads/main":      oid(2),
			"refs/tags/\xff\xfev1": oid(7),
		}
		in := corestore.CheckpointInput{Seq: 2, Refs: refs, PackKey: ownedKey(repo.ID, "checkpoint 2"), PackDeadline: time.Now().Add(time.Hour), PackBytes: 42}

		cp, created, err := s.CreateCheckpoint(ctx, repo.ID, in)
		require.NoError(t, err)
		require.True(t, created)
		require.Equal(t, uint64(2), cp.Seq)
		require.Equal(t, in.PackKey, cp.PackKey)
		require.Equal(t, int64(42), cp.PackBytes)
		require.False(t, cp.CreatedAt.IsZero())

		got, err := s.CheckpointRefs(ctx, repo.ID, 2)
		require.NoError(t, err)
		require.Equal(t, refs, got, "the snapshot round-trips, non-UTF-8 names and symbolic targets included")

		newest, ok, err := s.NewestCheckpoint(ctx, repo.ID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, cp, newest)

		// A second build at the same sequence keeps the first checkpoint and its pack.
		again, created, err := s.CreateCheckpoint(ctx, repo.ID, corestore.CheckpointInput{
			Seq: 2, Refs: refs, PackKey: ownedKey(repo.ID, "another build of 2"), PackDeadline: time.Now().Add(time.Hour),
		})
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, cp, again)

		for name, bad := range map[string]corestore.CheckpointInput{
			"sequence 0":            {Seq: 0, Refs: refs, PackKey: ownedKey(repo.ID, "x"), PackDeadline: time.Now().Add(time.Hour)},
			"beyond the head":       {Seq: 4, Refs: refs, PackKey: ownedKey(repo.ID, "x"), PackDeadline: time.Now().Add(time.Hour)},
			"older than the newest": {Seq: 1, Refs: refs, PackKey: ownedKey(repo.ID, "x"), PackDeadline: time.Now().Add(time.Hour)},
			"legacy pack key":       {Seq: 3, Refs: refs, PackKey: legacyKey(repo.ID, "x"), PackDeadline: time.Now().Add(time.Hour)},
			"another repository":    {Seq: 3, Refs: refs, PackKey: ownedKey(repo.ID+1000, "x"), PackDeadline: time.Now().Add(time.Hour)},
			"no pack key":           {Seq: 3, Refs: refs},
			"no references":         {Seq: 3, PackKey: ownedKey(repo.ID, "x"), PackDeadline: time.Now().Add(time.Hour)},
			"absent target":         {Seq: 3, Refs: map[string]corestore.Target{"refs/heads/main": ""}, PackKey: ownedKey(repo.ID, "x"), PackDeadline: time.Now().Add(time.Hour)},
			"empty name":            {Seq: 3, Refs: map[string]corestore.Target{"": oid(1)}, PackKey: ownedKey(repo.ID, "x"), PackDeadline: time.Now().Add(time.Hour)},
		} {
			_, _, err := s.CreateCheckpoint(ctx, repo.ID, bad)
			require.Error(t, err, name)

			newest, _, err := s.NewestCheckpoint(ctx, repo.ID)
			require.NoError(t, err)
			require.Equal(t, uint64(2), newest.Seq, "%s wrote nothing", name)
		}

		cp3, created, err := s.CreateCheckpoint(ctx, repo.ID, corestore.CheckpointInput{
			Seq: 3, Refs: map[string]corestore.Target{"refs/heads/main": oid(3)}, PackKey: ownedKey(repo.ID, "checkpoint 3"), PackDeadline: time.Now().Add(time.Hour),
		})
		require.NoError(t, err)
		require.True(t, created)

		st, err := s.GCSnapshot(ctx, repo.ID)
		require.NoError(t, err)
		require.Equal(t, []corestore.Checkpoint{cp, cp3}, st.Checkpoints)

		deleted, err := s.DeleteCheckpoint(ctx, repo.ID, 2)
		require.NoError(t, err)
		require.True(t, deleted)
		_, err = s.CheckpointRefs(ctx, repo.ID, 2)
		require.ErrorIs(t, err, corestore.ErrCheckpointNotFound)
		deleted, err = s.DeleteCheckpoint(ctx, repo.ID, 2)
		require.NoError(t, err)
		require.False(t, deleted)

		_, _, err = s.CreateCheckpoint(ctx, repo.ID+1000, corestore.CheckpointInput{
			Seq: 1, Refs: refs, PackKey: ownedKey(repo.ID+1000, "unknown"), PackDeadline: time.Now().Add(time.Hour),
		})
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
	})
}

func TestSyncBoundaryUsesTheDatabaseClock(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo, _ := headRepo(t, ctx, s, "boundary", 3)

		boundary, err := s.SyncBoundary(ctx, repo.ID, 0)
		require.NoError(t, err)
		require.Zero(t, boundary, "no checkpoint")

		for seq := uint64(1); seq <= 3; seq++ {
			_, _, err := s.CreateCheckpoint(ctx, repo.ID, corestore.CheckpointInput{
				Seq: seq, Refs: map[string]corestore.Target{"refs/heads/main": oid(int(seq))}, PackKey: ownedKey(repo.ID, fmt.Sprint(seq)), PackDeadline: time.Now().Add(time.Hour),
			})
			require.NoError(t, err)
		}
		now := time.Now()
		execSQL(t, ctx, s, "UPDATE graft_checkpoints SET created_at = ? WHERE repo_id = ? AND seq = ?", timeValue(s, now.Add(-3*time.Hour)), repo.ID, 1)
		execSQL(t, ctx, s, "UPDATE graft_checkpoints SET created_at = ? WHERE repo_id = ? AND seq = ?", timeValue(s, now.Add(-90*time.Minute)), repo.ID, 2)

		for _, tc := range []struct {
			age  time.Duration
			want uint64
		}{
			{age: 0, want: 3},
			{age: time.Hour, want: 2},
			{age: 2 * time.Hour, want: 1},
			{age: 4 * time.Hour, want: 0},
		} {
			boundary, err := s.SyncBoundary(ctx, repo.ID, tc.age)
			require.NoError(t, err)
			require.Equal(t, tc.want, boundary, "age %s", tc.age)
		}

		_, err = s.SyncBoundary(ctx, repo.ID, -time.Second)
		require.Error(t, err)
		_, err = s.SyncBoundary(ctx, repo.ID+1000, 0)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
	})
}

func TestOrphanObservations(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo, keys := headRepo(t, ctx, s, "orphans", 2)
		x, y := ownedKey(repo.ID, "x"), "repos/1/packs/leftover.tmp"

		require.NoError(t, s.RecordOrphans(ctx, repo.ID, []string{x, y}))

		st, err := s.GCSnapshot(ctx, repo.ID)
		require.NoError(t, err)
		require.Equal(t, map[string]uint64{keys[0]: 1, keys[1]: 2}, st.Packs)
		require.Len(t, st.Orphans, 2)
		for key, seen := range st.Orphans {
			require.False(t, seen.After(st.Now), "%s was observed before the snapshot", key)
		}

		// A second observation keeps the first one's time.
		old := time.Now().Add(-2 * time.Hour)
		execSQL(t, ctx, s, "UPDATE graft_gc_orphans SET first_seen = ? WHERE repo_id = ? AND pack_key = ?", timeValue(s, old), repo.ID, x)
		require.NoError(t, s.RecordOrphans(ctx, repo.ID, []string{x}))
		st, err = s.GCSnapshot(ctx, repo.ID)
		require.NoError(t, err)
		require.WithinDuration(t, old, st.Orphans[x], time.Millisecond)

		require.NoError(t, s.ForgetOrphans(ctx, repo.ID, []string{x}))
		st, err = s.GCSnapshot(ctx, repo.ID)
		require.NoError(t, err)
		require.NotContains(t, st.Orphans, x)
		require.Contains(t, st.Orphans, y)

		require.Error(t, s.RecordOrphans(ctx, repo.ID, []string{"repos/1/packs/\xff.pack"}), "an unrecordable key is an error")
		require.NoError(t, s.RecordOrphans(ctx, repo.ID, nil))
		require.NoError(t, s.ForgetOrphans(ctx, repo.ID, nil))

		_, err = s.GCSnapshot(ctx, repo.ID+1000)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
	})
}

func TestMaintenanceLease(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, a *sqlstore.Store) {
		// Two nodes on one database.
		b := openStore(t, ctx, coreDSN(t, a))
		repo, _ := headRepo(t, ctx, a, "lease", 1)
		ttl := time.Hour

		acquire := func(s *sqlstore.Store, holder string) bool {
			t.Helper()
			ok, err := s.AcquireMaintenanceLease(ctx, repo.ID, holder, ttl)
			require.NoError(t, err)
			return ok
		}
		renew := func(s *sqlstore.Store, holder string) bool {
			t.Helper()
			ok, err := s.RenewMaintenanceLease(ctx, repo.ID, holder, ttl)
			require.NoError(t, err)
			return ok
		}

		require.True(t, acquire(a, "node-a"))
		require.False(t, acquire(b, "node-b"), "held by node-a")
		require.True(t, acquire(a, "node-a"), "the holder acquires again")
		require.False(t, renew(b, "node-b"))
		require.True(t, renew(a, "node-a"))

		require.NoError(t, b.ReleaseMaintenanceLease(ctx, repo.ID, "node-b"), "releasing a lease one does not hold changes nothing")
		require.False(t, acquire(b, "node-b"))
		require.NoError(t, a.ReleaseMaintenanceLease(ctx, repo.ID, "node-a"))
		require.True(t, acquire(b, "node-b"))

		// An expired lease (its holder died) can be taken over.
		execSQL(t, ctx, a, "UPDATE graft_gc_leases SET expires_at = ? WHERE repo_id = ?", timeValue(a, time.Now().Add(-time.Minute)), repo.ID)
		require.True(t, acquire(a, "node-a"))
		require.False(t, renew(b, "node-b"), "node-b lost the lease")

		_, err := a.AcquireMaintenanceLease(ctx, repo.ID, "node-a", 0)
		require.Error(t, err)
		_, err = a.AcquireMaintenanceLease(ctx, repo.ID, "", ttl)
		require.Error(t, err)
		_, err = a.AcquireMaintenanceLease(ctx, repo.ID+1000, "node-a", ttl)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
		_, err = a.RenewMaintenanceLease(ctx, repo.ID+1000, "node-a", ttl)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
		require.NoError(t, a.ReleaseMaintenanceLease(ctx, repo.ID+1000, "node-a"))

		// Exactly one of concurrent acquisitions from both nodes wins.
		require.NoError(t, a.ReleaseMaintenanceLease(ctx, repo.ID, "node-a"))
		var winners atomic.Int32
		var g errgroup.Group
		for i := range 16 {
			s := a
			if i%2 == 1 {
				s = b
			}
			g.Go(func() error {
				ok, err := s.AcquireMaintenanceLease(ctx, repo.ID, fmt.Sprintf("node-%d", i), ttl)
				if ok {
					winners.Add(1)
				}
				return err
			})
		}
		require.NoError(t, g.Wait())
		require.Equal(t, int32(1), winners.Load())
	})
}

func TestDeleteRepositoryRecordsTombstone(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo, _ := headRepo(t, ctx, s, "removed", 2)
		key := testKey("removed")

		// Rows of milestone 3 go with the repository.
		_, _, err := s.CreateCheckpoint(ctx, repo.ID, corestore.CheckpointInput{
			Seq: 2, Refs: map[string]corestore.Target{"refs/heads/main": oid(2)}, PackKey: ownedKey(repo.ID, "cp"), PackDeadline: time.Now().Add(time.Hour),
		})
		require.NoError(t, err)
		require.NoError(t, s.RecordOrphans(ctx, repo.ID, []string{ownedKey(repo.ID, "orphan")}))
		ok, err := s.AcquireMaintenanceLease(ctx, repo.ID, "node-a", time.Hour)
		require.NoError(t, err)
		require.True(t, ok)

		deleted, err := s.DeleteRepository(ctx, key)
		require.NoError(t, err)
		require.True(t, deleted)

		_, err = s.GCSnapshot(ctx, repo.ID)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
		for _, table := range []string{"graft_checkpoints", "graft_checkpoint_refs", "graft_gc_orphans", "graft_gc_leases", "graft_changesets"} {
			var n int
			query := "SELECT COUNT(*) FROM " + table + " WHERE repo_id = ?"
			if s.Dialect() == sqlstore.Postgres {
				query = strings.Replace(query, "?", "$1", 1)
			}
			require.NoError(t, s.DB().QueryRowContext(ctx, query, repo.ID).Scan(&n))
			require.Zero(t, n, "%s rows of the removed repository", table)
		}

		removed, err := s.ListRemovedRepositories(ctx, "", 0)
		require.NoError(t, err)
		require.Len(t, removed, 1)
		require.Equal(t, repo.ID, removed[0].RepoID)
		require.Equal(t, key, removed[0].Key)

		// The age filter is on the database clock.
		removed, err = s.ListRemovedRepositories(ctx, "default", time.Hour)
		require.NoError(t, err)
		require.Empty(t, removed, "too young")
		execSQL(t, ctx, s, "UPDATE graft_gc_removed SET removed_at = ? WHERE repo_id = ?", timeValue(s, time.Now().Add(-2*time.Hour)), repo.ID)
		removed, err = s.ListRemovedRepositories(ctx, "default", time.Hour)
		require.NoError(t, err)
		require.Len(t, removed, 1)
		removed, err = s.ListRemovedRepositories(ctx, "other-storage", 0)
		require.NoError(t, err)
		require.Empty(t, removed)

		// Deleting an unknown repository records nothing.
		deleted, err = s.DeleteRepository(ctx, key)
		require.NoError(t, err)
		require.False(t, deleted)
		removed, err = s.ListRemovedRepositories(ctx, "", 0)
		require.NoError(t, err)
		require.Len(t, removed, 1)

		// A repository created again at the path is a new one.
		again := register(t, ctx, s, key)
		require.NotEqual(t, repo.ID, again.ID)

		require.NoError(t, s.DeleteRemovedRepository(ctx, repo.ID))
		require.NoError(t, s.DeleteRemovedRepository(ctx, repo.ID), "idempotent")
		removed, err = s.ListRemovedRepositories(ctx, "", 0)
		require.NoError(t, err)
		require.Empty(t, removed)
	})
}

func TestListRepositories(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		register(t, ctx, s, testKey("incomplete-import")) // head sequence 0
		first, _ := headRepo(t, ctx, s, "first", 1)
		second, _ := headRepo(t, ctx, s, "second", 2)

		otherKey := corestore.RepoKey{Storage: "other", RelativePath: "@hashed/other.git"}
		other, _, err := s.RegisterRepository(ctx, otherKey, "sha256", corestore.InitialState{})
		require.NoError(t, err)
		_, err = commit(ctx, s, other.ID, create("HEAD", corestore.SymbolicTarget("refs/heads/main")))
		require.NoError(t, err)

		ids := func(storage string) []int64 {
			repos, err := s.ListRepositories(ctx, storage)
			require.NoError(t, err)
			var ids []int64
			for _, r := range repos {
				require.NotZero(t, r.HeadSeq)
				ids = append(ids, r.ID)
			}
			return ids
		}

		require.Equal(t, []int64{first.ID, second.ID, other.ID}, ids(""))
		require.Equal(t, []int64{first.ID, second.ID}, ids("default"))
		require.Equal(t, []int64{other.ID}, ids("other"))
		require.Empty(t, ids("missing"))
	})
}

// TestLockOrderConcurrentWriters runs changesets, review creations, review
// updates, checkpoint commits and lease operations on one repository from two
// nodes at once: every transaction that serializes on the repository takes its
// row first (doc/graft/PLAN7.md, LOCK ORDER), so Postgres never reports a
// deadlock, and every invariant holds at the end.
func TestLockOrderConcurrentWriters(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, a *sqlstore.Store) {
		b := openStore(t, ctx, coreDSN(t, a))
		repo, _ := headRepo(t, ctx, a, "lock-order", 1)

		var deadlocks atomic.Int32
		restore := sqlstore.SetRetryHook(func(err error) {
			if sqlstore.IsDeadlock(err) {
				deadlocks.Add(1)
			}
		})
		defer restore()

		_, err := a.InsertReview(ctx, repo.ID, func(string) (*gitalypb.Review, error) {
			return &gitalypb.Review{TargetBranch: []byte("main"), HeadCommitId: string(oid(500))}, nil
		}, false)
		require.NoError(t, err)

		const (
			landsPerNode   = 30
			reviewsPerNode = 15
			updates        = 15
			checkpoints    = 10
		)

		var (
			mu               sync.Mutex
			createdSeqs      []uint64
			rootsMissing     int
			swept, inUse     int
			committed        atomic.Int32
			unexpected       []error
			recordUnexpected = func(err error) { mu.Lock(); unexpected = append(unexpected, err); mu.Unlock() }
			nodes            = []*sqlstore.Store{a, b}
			g                errgroup.Group
		)

		// protect writes the keep-around reference of the commit, as the review
		// service does through the voter; a reference that exists is a conflict
		// and left as it is.
		protect := func(s *sqlstore.Store, root string) error {
			_, err := commit(ctx, s, repo.ID, create(corestore.KeepAroundRef(root), corestore.Target(root)))
			var conflict *corestore.ErrRefConflict
			switch {
			case err == nil:
				committed.Add(1)
				return nil
			case errors.As(err, &conflict):
				return nil
			default:
				return err
			}
		}

		// writeRoots runs a review write that records commit as the review
		// service does (REVIEW ROOTS RULE): its reference first, then the write
		// that requires it, again after ErrReviewRootsMissing (the sweeper below
		// deleted the reference in between). Its own bound is generous: the test
		// counts the retries instead of failing on the service's third attempt.
		writeRoots := func(s *sqlstore.Store, root string, write func() error) error {
			for range 20 {
				if err := protect(s, root); err != nil {
					return err
				}
				err := write()
				if !errors.Is(err, corestore.ErrReviewRootsMissing) {
					return err
				}
				mu.Lock()
				rootsMissing++
				mu.Unlock()
			}
			return errors.New("the review write kept losing its keep-around reference")
		}

		// The review writers below; the sweeper runs until they are done, so that
		// it races with every one of them.
		var writers sync.WaitGroup
		writersDone := make(chan struct{})

		for n, s := range nodes {
			g.Go(func() error {
				for i := range landsPerNode {
					ref := fmt.Sprintf("refs/heads/node-%d-%d", n, i)
					if _, err := commitPack(ctx, s, repo.ID, create(ref, oid(1000+i)), ownedKey(repo.ID, ref)); err != nil {
						recordUnexpected(fmt.Errorf("land %s: %w", ref, err))
						continue
					}
					committed.Add(1)
				}
				return nil
			})
			writers.Add(1)
			g.Go(func() error {
				defer writers.Done()
				for i := range reviewsPerNode {
					head := string(oid(2000 + 100*n + i))
					if err := writeRoots(s, head, func() error {
						_, err := s.InsertReview(ctx, repo.ID, func(string) (*gitalypb.Review, error) {
							return &gitalypb.Review{TargetBranch: []byte("main"), HeadCommitId: head}, nil
						}, true)
						return err
					}); err != nil {
						recordUnexpected(fmt.Errorf("insert review: %w", err))
					}
				}
				return nil
			})
		}

		// The sweeper deletes every keep-around reference it finds; the database
		// refuses those that an open review uses.
		g.Go(func() error {
			for {
				select {
				case <-writersDone:
					return nil
				default:
				}

				refs, _, err := b.Refs(ctx, repo.ID)
				if err != nil {
					recordUnexpected(err)
					return nil
				}
				for name, target := range refs {
					if !strings.HasPrefix(name, corestore.KeepAroundPrefix) {
						continue
					}
					_, err := commit(ctx, b, repo.ID, update(name, target, ""))
					var conflict *corestore.ErrRefConflict
					switch {
					case err == nil:
						committed.Add(1)
						mu.Lock()
						swept++
						mu.Unlock()
					case errors.Is(err, corestore.ErrReviewRootInUse):
						mu.Lock()
						inUse++
						mu.Unlock()
					case errors.As(err, &conflict):
					default:
						recordUnexpected(fmt.Errorf("sweep %s: %w", name, err))
					}
				}
			}
		})

		// Head moves of review 1: the repository row first, then the version
		// compare-and-swap of the review row.
		writers.Add(1)
		g.Go(func() error {
			defer writers.Done()
			for done := 0; done < updates; {
				head := string(oid(3000 + done))
				err := writeRoots(a, head, func() error {
					r, version, err := a.GetReview(ctx, repo.ID, "1")
					if err != nil {
						return err
					}
					r.HeadCommitId = head
					return a.UpdateReview(ctx, repo.ID, "1", version, r, true)
				})
				switch {
				case err == nil:
					done++
				case errors.Is(err, corestore.ErrReviewVersionConflict):
				default:
					recordUnexpected(fmt.Errorf("move review head: %w", err))
					return nil
				}
			}
			return nil
		})
		go func() {
			writers.Wait()
			close(writersDone)
		}()

		// Updates of one review that do not move its commits (a comment): a
		// version compare-and-swap on the review row only.
		g.Go(func() error {
			for done := 0; done < updates; {
				r, version, err := b.GetReview(ctx, repo.ID, "1")
				if err != nil {
					recordUnexpected(fmt.Errorf("get review: %w", err))
					return nil
				}
				r.Title = []byte(fmt.Sprintf("comment %d", done))
				switch err := b.UpdateReview(ctx, repo.ID, "1", version, r, false); {
				case err == nil:
					done++
				case errors.Is(err, corestore.ErrReviewVersionConflict):
				default:
					recordUnexpected(fmt.Errorf("update review: %w", err))
					return nil
				}
			}
			return nil
		})

		// Checkpoints at the head.
		g.Go(func() error {
			for range checkpoints {
				refs, head, err := a.Refs(ctx, repo.ID)
				if err != nil {
					recordUnexpected(err)
					return nil
				}

				cp, created, err := a.CreateCheckpoint(ctx, repo.ID, corestore.CheckpointInput{
					Seq: head, Refs: refs, PackKey: ownedKey(repo.ID, fmt.Sprint("checkpoint ", head)), PackDeadline: time.Now().Add(time.Hour),
				})
				switch {
				case err == nil && created:
					mu.Lock()
					createdSeqs = append(createdSeqs, cp.Seq)
					mu.Unlock()
				case err == nil:
				default:
					recordUnexpected(fmt.Errorf("checkpoint at %d: %w", head, err))
				}
			}
			return nil
		})

		g.Go(func() error {
			for i := range 20 {
				holder := fmt.Sprintf("node-%d", i%2)
				if _, err := nodes[i%2].AcquireMaintenanceLease(ctx, repo.ID, holder, time.Minute); err != nil {
					recordUnexpected(fmt.Errorf("acquire lease: %w", err))
				}
				if err := nodes[i%2].ReleaseMaintenanceLease(ctx, repo.ID, holder); err != nil {
					recordUnexpected(fmt.Errorf("release lease: %w", err))
				}
			}
			return nil
		})

		require.NoError(t, g.Wait())
		require.Empty(t, unexpected)
		require.Zero(t, deadlocks.Load(), "a transaction deadlocked")

		// Dense changesets: the first one plus every land, keep-around write and
		// sweep that committed.
		final, err := a.GetRepository(ctx, testKey("lock-order"))
		require.NoError(t, err)
		require.Equal(t, uint64(1+committed.Load()), final.HeadSeq)
		changesets, err := a.ListChangesets(ctx, repo.ID, 0, final.HeadSeq)
		require.NoError(t, err)
		require.Len(t, changesets, int(final.HeadSeq))

		// Dense review IDs: the first one plus every creation.
		reviews, err := a.ListReviews(ctx, repo.ID, "", true, 0)
		require.NoError(t, err)
		require.Len(t, reviews, 1+2*reviewsPerNode)

		// Checkpoints are increasing and never beyond the head; every one the
		// checkpoint goroutine saw created is recorded.
		st, err := a.GCSnapshot(ctx, repo.ID)
		require.NoError(t, err)
		var recorded []uint64
		for i, cp := range st.Checkpoints {
			require.LessOrEqual(t, cp.Seq, final.HeadSeq)
			if i > 0 {
				require.Greater(t, cp.Seq, st.Checkpoints[i-1].Seq)
			}
			recorded = append(recorded, cp.Seq)
		}
		require.Equal(t, createdSeqs, recorded)
		t.Logf("checkpoints created %d; keep-around references swept %d, kept for an open review %d; review writes retried after a sweep %d",
			len(createdSeqs), swept, inUse, rootsMissing)

		r, _, err := a.GetReview(ctx, repo.ID, "1")
		require.NoError(t, err)
		require.Equal(t, string(oid(3000+updates-1)), r.GetHeadCommitId(), "every head move applied")

		// The REVIEW ROOTS invariant: every open review written with its roots
		// required has the keep-around reference of its head. Review 1 was
		// inserted without, but every head move required its new head.
		refs, _, err := a.Refs(ctx, repo.ID)
		require.NoError(t, err)
		for _, review := range reviews {
			head := review.GetHeadCommitId()
			require.Equal(t, corestore.Target(head), refs[corestore.KeepAroundRef(head)], "review %s", review.GetId())
		}
	})
}
