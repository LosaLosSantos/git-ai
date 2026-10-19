package sqlstore_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestConcurrentCheckpointsOfOneSequence verifies that builds of the same
// checkpoint racing each other (a CreateCheckpoint request and the background
// maintenance, two nodes) all succeed: one records it, the others get it back
// (integrated review m7). The record does not lock the repository row, so on
// Postgres they meet at the primary key.
func TestConcurrentCheckpointsOfOneSequence(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo, _ := headRepo(t, ctx, s, "racing checkpoints", 1)
		const builds = 6

		type result struct {
			cp      corestore.Checkpoint
			created bool
			err     error
		}
		start := make(chan struct{})
		results := make(chan result, builds)
		for i := 0; i < builds; i++ {
			go func() {
				<-start
				cp, created, err := s.CreateCheckpoint(ctx, repo.ID, corestore.CheckpointInput{
					Seq: 1, Refs: map[string]corestore.Target{"refs/heads/main": oid(1)},
					PackKey: ownedKey(repo.ID, fmt.Sprint("build ", i)), PackBytes: 1, PackDeadline: time.Now().Add(time.Hour),
				})
				results <- result{cp: cp, created: created, err: err}
			}()
		}
		close(start)

		var recorded []corestore.Checkpoint
		created := 0
		for i := 0; i < builds; i++ {
			r := <-results
			require.NoError(t, r.err)
			if r.created {
				created++
			}
			recorded = append(recorded, r.cp)
		}
		require.Equal(t, 1, created, "one build records the checkpoint")
		for _, cp := range recorded {
			require.Equal(t, recorded[0], cp, "every build gets the recorded checkpoint")
		}
	})
}

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
