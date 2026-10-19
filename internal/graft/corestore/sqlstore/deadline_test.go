package sqlstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestCommitChecksThePackDeadlineOnTheDatabaseClock verifies the write
// deadline of a pack in the commit transaction (integrated review m6). A
// writer's monotonic clock stops while its host is suspended or its VM paused,
// so the deadline it measured may not have expired for it when, on the
// database clock that garbage collection uses, it has: such a commit is
// refused, and nothing is written. A row with a pack and no deadline is
// refused too.
func TestCommitChecksThePackDeadlineOnTheDatabaseClock(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("deadline"))
		record, err := s.GetRepository(ctx, repo.Key)
		require.NoError(t, err)
		require.False(t, record.ReadAt.IsZero(), "GetRepository returns the database time of the read")
		require.WithinDuration(t, time.Now(), record.ReadAt, time.Minute)

		changeset := func(deadline time.Time) corestore.ChangesetInput {
			return corestore.ChangesetInput{
				RepoID:       repo.ID,
				Updates:      []corestore.RefUpdate{create("refs/heads/main", oid(1))},
				Operation:    "test",
				Actor:        "tester",
				NodeID:       "node-a",
				PackKey:      ownedKey(repo.ID, "pack"),
				PackBytes:    1,
				PackDeadline: deadline,
			}
		}

		// The writer slept through its deadline: the database clock is past it.
		_, err = s.CommitChangeset(ctx, changeset(record.ReadAt.Add(-time.Second)))
		require.ErrorIs(t, err, corestore.ErrPackDeadline)
		_, err = s.CommitChangeset(ctx, changeset(time.Time{}))
		require.ErrorContains(t, err, "must carry the pack's write deadline")
		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{})

		res, err := s.CommitChangeset(ctx, changeset(record.ReadAt.Add(time.Hour)))
		require.NoError(t, err)
		require.Equal(t, uint64(1), res.Seq)

		checkpoint := func(deadline time.Time) corestore.CheckpointInput {
			return corestore.CheckpointInput{
				Seq: 1, Refs: map[string]corestore.Target{"refs/heads/main": oid(1)},
				PackKey: ownedKey(repo.ID, "checkpoint"), PackBytes: 1, PackDeadline: deadline,
			}
		}
		_, _, err = s.CreateCheckpoint(ctx, repo.ID, checkpoint(record.ReadAt.Add(-time.Second)))
		require.ErrorIs(t, err, corestore.ErrPackDeadline)
		_, ok, err := s.NewestCheckpoint(ctx, repo.ID)
		require.NoError(t, err)
		require.False(t, ok, "nothing was recorded")
		_, created, err := s.CreateCheckpoint(ctx, repo.ID, checkpoint(record.ReadAt.Add(time.Hour)))
		require.NoError(t, err)
		require.True(t, created)
	})
}
