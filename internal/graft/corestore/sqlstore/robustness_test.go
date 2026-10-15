package sqlstore_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/coretest"
	"github.com/feder-cr/open_scion/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// holdRepositoryLock opens a transaction on other that holds the lock a
// changeset of the repository needs: on SQLite the database write lock (the
// immediate transaction), on Postgres the repository row. The returned
// function releases it.
func holdRepositoryLock(t *testing.T, ctx context.Context, other *sqlstore.Store, repoID int64) func() {
	t.Helper()

	tx, err := other.DB().BeginTx(ctx, nil)
	require.NoError(t, err)

	query := "UPDATE graft_repositories SET next_review_id = next_review_id WHERE repo_id = ?"
	if other.Dialect() == sqlstore.Postgres {
		query = "UPDATE graft_repositories SET next_review_id = next_review_id WHERE repo_id = $1"
	}
	_, err = tx.ExecContext(ctx, query, repoID)
	require.NoError(t, err)

	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)

	return release
}

// requireReturnsBy runs fn and checks that it returns no later than limit after
// it started.
func requireReturnsBy(t *testing.T, limit time.Duration, fn func() error) error {
	t.Helper()

	start := time.Now()
	err := fn()
	elapsed := time.Since(start)
	require.Less(t, elapsed, limit, "call returned after %s (error: %v)", elapsed, err)

	return err
}

func TestWritesHonourDeadlineWhileLocked(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("deadline"))
		_, err := commit(ctx, s, repo.ID, create("refs/heads/main", oid(1)))
		require.NoError(t, err)

		other := openStore(t, ctx, coreDSN(t, s))
		release := holdRepositoryLock(t, ctx, other, repo.ID)

		for _, tc := range []struct {
			desc string
			call func(ctx context.Context) error
		}{
			{
				desc: "CommitChangeset",
				call: func(ctx context.Context) error {
					_, err := commit(ctx, s, repo.ID, update("refs/heads/main", oid(1), oid(2)))
					return err
				},
			},
			{
				desc: "InsertReview",
				call: func(ctx context.Context) error {
					_, err := s.InsertReview(ctx, repo.ID, func(string) (*gitalypb.Review, error) {
						return newReview("t", "refs/heads/main", gitalypb.Review_STATE_OPEN_UNSPECIFIED), nil
					})
					return err
				},
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				callCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()

				err := requireReturnsBy(t, 1300*time.Millisecond, func() error { return tc.call(callCtx) })
				require.Error(t, err)
				require.ErrorIs(t, err, context.DeadlineExceeded)
			})
		}

		// A canceled context without a deadline ends the wait as well.
		cancelCtx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(200*time.Millisecond, cancel)
		defer timer.Stop()
		err = requireReturnsBy(t, 1200*time.Millisecond, func() error {
			_, err := commit(cancelCtx, s, repo.ID, update("refs/heads/main", oid(1), oid(2)))
			return err
		})
		require.ErrorIs(t, err, context.Canceled)

		// Nothing was written while the lock was held.
		release()
		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{"refs/heads/main": oid(1)})
	})
}

func TestReadsDoNotWaitForWriters(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("readers"))
		_, err := commit(ctx, s, repo.ID, create("refs/heads/main", oid(1)))
		require.NoError(t, err)

		other := openStore(t, ctx, coreDSN(t, s))
		holdRepositoryLock(t, ctx, other, repo.ID)

		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		require.NoError(t, requireReturnsBy(t, time.Second, func() error {
			refs, head, err := s.Refs(readCtx, repo.ID)
			if err == nil {
				require.Equal(t, map[string]corestore.Target{"refs/heads/main": oid(1)}, refs)
				require.Equal(t, uint64(1), head)
			}
			return err
		}))

		require.NoError(t, requireReturnsBy(t, time.Second, func() error {
			changesets, err := s.ListChangesets(readCtx, repo.ID, 0, 10)
			if err == nil {
				require.Len(t, changesets, 1)
			}
			return err
		}))

		require.NoError(t, requireReturnsBy(t, time.Second, func() error {
			_, err := s.GetRepository(readCtx, testKey("readers"))
			return err
		}))
	})
}

func TestCommitWaitsForLockWithoutDeadline(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("wait"))
		_, err := commit(ctx, s, repo.ID, create("refs/heads/main", oid(1)))
		require.NoError(t, err)

		other := openStore(t, ctx, coreDSN(t, s))

		// A first commit times out, which must not leave the connection with a
		// short busy timeout: the next commit waits for the lock as long as it
		// is held (here 700 ms, several busy slices).
		release := holdRepositoryLock(t, ctx, other, repo.ID)
		shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		_, err = commit(shortCtx, s, repo.ID, update("refs/heads/main", oid(1), oid(2)))
		cancel()
		require.Error(t, err)

		timer := time.AfterFunc(700*time.Millisecond, release)
		defer timer.Stop()

		res, err := commit(ctx, s, repo.ID, update("refs/heads/main", oid(1), oid(2)))
		require.NoError(t, err)
		require.Equal(t, uint64(2), res.Seq)

		// Plain reads of the pool still wait for locks normally.
		_, err = s.GetRepository(ctx, testKey("wait"))
		require.NoError(t, err)
	})
}

func TestConcurrentOpenOfNewDatabase(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			ctx := testhelper.Context(t)
			dsn := coretest.DatabaseURL(t, dialect)

			const openers = 8
			stores := make([]*sqlstore.Store, openers)
			errs := make([]error, openers)

			var wg sync.WaitGroup
			for i := 0; i < openers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					stores[i], errs[i] = sqlstore.Open(ctx, dsn)
				}(i)
			}
			wg.Wait()

			for i := 0; i < openers; i++ {
				require.NoError(t, errs[i], "opener %d", i)
				defer func(s *sqlstore.Store) { require.NoError(t, s.Close()) }(stores[i])
			}

			repo, _, err := stores[0].RegisterRepository(ctx, testKey("open"), "sha1", corestore.InitialState{})
			require.NoError(t, err)
			got, err := stores[openers-1].GetRepository(ctx, testKey("open"))
			require.NoError(t, err)
			require.Equal(t, repo.ID, got.ID)
		})
	}
}

func TestCursorsAboveMaxInt64(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("cursors"))
		for i := 1; i <= 3; i++ {
			_, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
				RepoID:    repo.ID,
				Updates:   []corestore.RefUpdate{force("refs/heads/main", oid(i))},
				Operation: "test",
				Actor:     "tester",
				Changelog: &changelog.Meta{Operation: "test", Actor: "tester"},
			})
			require.NoError(t, err)
		}

		changesets, err := s.ListChangesets(ctx, repo.ID, 0, math.MaxUint64)
		require.NoError(t, err)
		require.Len(t, changesets, 3)

		changesets, err = s.ListChangesets(ctx, repo.ID, 2, math.MaxInt64+1)
		require.NoError(t, err)
		require.Len(t, changesets, 1)
		require.Equal(t, uint64(3), changesets[0].Seq)

		for _, after := range []uint64{math.MaxInt64, math.MaxInt64 + 1, math.MaxUint64 - 1} {
			changesets, err = s.ListChangesets(ctx, repo.ID, after, math.MaxUint64)
			require.NoError(t, err)
			require.Empty(t, changesets, "after %d", after)

			entries, err := s.ScanChangelog(ctx, repo.ID, after, 0)
			require.NoError(t, err)
			require.Empty(t, entries, "after %d", after)
		}

		entries, err := s.ScanChangelog(ctx, repo.ID, math.MaxUint64, 10)
		require.NoError(t, err)
		require.Empty(t, entries)

		entries, err = s.ScanChangelog(ctx, repo.ID, 1, 0)
		require.NoError(t, err)
		require.Len(t, entries, 2)
	})
}

func TestTextWithNULBytesIsStoredAlike(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("nul"))

		res, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID:    repo.ID,
			Updates:   []corestore.RefUpdate{create("refs/heads/main", oid(1))},
			Operation: "op\x00x",
			Actor:     "ev\x00il\xff",
			NodeID:    "node\x00a",
			Changelog: &changelog.Meta{Operation: "op\x00x", Actor: "ev\x00il\xff"},
		})
		require.NoError(t, err)
		require.Len(t, res.Changelog, 1)

		changesets, err := s.ListChangesets(ctx, repo.ID, 0, 1)
		require.NoError(t, err)
		require.Len(t, changesets, 1)
		require.Equal(t, "op�x", changesets[0].Operation)
		require.Equal(t, "ev�il�", changesets[0].Actor)
		require.Equal(t, "node�a", changesets[0].NodeID)

		entries, err := s.ScanChangelog(ctx, repo.ID, 0, 0)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, "ev�il�", entries[0].Actor)
		require.Equal(t, "op�x", entries[0].Operation)

		require.NoError(t, s.ReportApplied(ctx, repo.ID, "node\x00b\xfe", 1))
	})
}

func TestTargetsThatAreNotTextAreRefused(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("targets"))

		for _, target := range []corestore.Target{
			corestore.SymbolicTarget("refs/heads/b\xff"),
			corestore.SymbolicTarget("refs/heads/b\x00c"),
		} {
			_, err := commit(ctx, s, repo.ID, create("HEAD", target))
			require.Error(t, err, "target %q", target)
			var conflict *corestore.ErrRefConflict
			require.False(t, errors.As(err, &conflict))
		}

		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{})
		changesets, err := s.ListChangesets(ctx, repo.ID, 0, 10)
		require.NoError(t, err)
		require.Empty(t, changesets)

		// A reference name that is not UTF-8 is a binary column and is accepted,
		// also as the target of a symbolic reference when it is UTF-8.
		_, err = commit(ctx, s, repo.ID,
			create("refs/heads/b\xff", oid(1)),
			create("HEAD", corestore.SymbolicTarget("refs/heads/main")),
		)
		require.NoError(t, err)
	})
}

func TestRepositoryKeysThatAreNotText(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		for _, key := range []corestore.RepoKey{
			{Storage: "default", RelativePath: "@hashed/bad\xff.git"},
			{Storage: "default", RelativePath: "@hashed/bad\x00.git"},
			{Storage: "def\x00ault", RelativePath: "@hashed/ok.git"},
		} {
			_, _, err := s.RegisterRepository(ctx, key, "sha1", corestore.InitialState{})
			require.Error(t, err)
			require.False(t, errors.Is(err, corestore.ErrRepositoryExists))

			_, err = s.GetRepository(ctx, key)
			require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)

			deleted, err := s.DeleteRepository(ctx, key)
			require.NoError(t, err)
			require.False(t, deleted)
		}
	})
}
