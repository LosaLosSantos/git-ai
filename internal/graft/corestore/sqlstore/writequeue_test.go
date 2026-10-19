package sqlstore_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// TestSQLiteWritersAreAdmittedInArrivalOrder checks that the write
// transactions of a SQLite store take the database's write lock in the order
// they arrived, not in the order SQLite's busy handler happens to pick
// (doc/graft/PLAN7.md, WP73b).
func TestSQLiteWritersAreAdmittedInArrivalOrder(t *testing.T) {
	ctx := testhelper.Context(t)
	s := openStore(t, ctx, coretest.DatabaseURL(t, sqlstore.SQLite))
	repo := register(t, ctx, s, testKey("fifo"))

	queued := make(chan int)
	defer sqlstore.SetQueuedHook(func(waiting int) { queued <- waiting })()

	release, err := s.HoldWriteAdmission(ctx)
	require.NoError(t, err)

	const writers = 8
	var g errgroup.Group
	for i := range writers {
		g.Go(func() error {
			_, err := commit(ctx, s, repo.ID, create(fmt.Sprintf("refs/heads/w%d", i), oid(i+1)))
			return err
		})
		// Writer i is in the queue, behind the i writers started before it,
		// before the next writer starts.
		require.Equal(t, i+1, <-queued)
	}

	release()
	require.NoError(t, g.Wait())

	changesets, err := s.ListChangesets(ctx, repo.ID, 0, writers)
	require.NoError(t, err)
	require.Len(t, changesets, writers)
	for i, cs := range changesets {
		require.Equal(t, fmt.Sprintf("refs/heads/w%d", i), cs.Updates[0].Name,
			"changeset %d is the write of the writer that arrived %d-th", cs.Seq, i)
	}
}

// TestSQLiteWriterLeavesTheQueueWhenItsContextEnds checks that a writer whose
// context ends while it waits leaves the queue at once, writes nothing, and
// does not hold up the writers that come after it.
func TestSQLiteWriterLeavesTheQueueWhenItsContextEnds(t *testing.T) {
	ctx := testhelper.Context(t)
	s := openStore(t, ctx, coretest.DatabaseURL(t, sqlstore.SQLite))
	repo := register(t, ctx, s, testKey("leave"))

	queued := make(chan int)
	defer sqlstore.SetQueuedHook(func(waiting int) { queued <- waiting })()

	release, err := s.HoldWriteAdmission(ctx)
	require.NoError(t, err)

	writerCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() {
		_, err := commit(writerCtx, s, repo.ID, create("refs/heads/gone", oid(1)))
		errc <- err
	}()
	require.Equal(t, 1, <-queued)

	cancel()
	require.ErrorIs(t, <-errc, context.Canceled)
	require.Zero(t, s.WaitingWriters(), "the writer left the queue")

	release()
	_, err = commit(ctx, s, repo.ID, create("refs/heads/next", oid(2)))
	require.NoError(t, err, "the admission is free again")
	requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{"refs/heads/next": oid(2)})
}
