package syncer_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// checkpoint records a checkpoint of repository id at its head, with a pack of
// everything its references reach built from src, as checkpoint.Create does.
func (e *env) checkpoint(t *testing.T, id int64, src source) corestore.Checkpoint {
	t.Helper()

	refs, seq, err := e.core.Meta.Refs(e.ctx, id)
	require.NoError(t, err)
	var tips []git.ObjectID
	for _, target := range refs {
		if !target.IsSymbolic() {
			tips = append(tips, git.ObjectID(target))
		}
	}
	pack, err := gitops.BuildPackFrom(e.ctx, src.repo, tips, t.TempDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, pack.Remove()) }()

	key := corestore.NewPackKey(id, pack.SHA256)
	rc, err := pack.Open()
	require.NoError(t, err)
	require.NoError(t, e.core.Blobs.Put(e.ctx, key, rc))
	require.NoError(t, rc.Close())

	cp, created, err := e.core.Meta.CreateCheckpoint(e.ctx, id, corestore.CheckpointInput{Seq: seq, Refs: refs, PackKey: key, PackDeadline: time.Now().Add(time.Hour), PackBytes: pack.Size})
	require.NoError(t, err)
	require.True(t, created)
	return cp
}

// collectCovered deletes the changeset packs up to seq from the bucket, as
// garbage collection does once a checkpoint at seq is C_gc.
func (e *env) collectCovered(t *testing.T, id int64, seq uint64) int {
	t.Helper()

	st, err := e.core.Meta.GCSnapshot(e.ctx, id)
	require.NoError(t, err)
	deleted := 0
	for key, last := range st.Packs {
		if last <= seq {
			require.NoError(t, e.core.Blobs.Delete(e.ctx, key))
			deleted++
		}
	}
	return deleted
}

// ageCheckpoint moves the creation of the checkpoint at seq d into the past on
// the database clock.
func (e *env) ageCheckpoint(t *testing.T, id int64, seq uint64, d time.Duration) {
	t.Helper()

	store := e.core.Meta.(*sqlstore.Store)
	when := time.Now().Add(-d).UTC()
	var value any = when
	query := "UPDATE graft_checkpoints SET created_at = ? WHERE repo_id = ? AND seq = ?"
	if store.Dialect() == sqlstore.SQLite {
		value = when.Format(time.RFC3339Nano)
	} else {
		query = "UPDATE graft_checkpoints SET created_at = $1 WHERE repo_id = $2 AND seq = $3"
	}
	_, err := store.DB().ExecContext(e.ctx, query, value, id, int64(seq))
	require.NoError(t, err)
}

// TestSyncerHydratesFromCheckpoint: a node with no copy hydrates from the
// newest checkpoint and replays only the changesets after it, so it works after
// garbage collection deleted the packs the checkpoint covers (CKPT-1).
func TestSyncerHydratesFromCheckpoint(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		a := newEnv(t, envOptions{dialect: dialect})
		src := a.newSource(t)
		c1 := a.writeCommit(t, src)
		repo := a.register(t, a.key(localRelPath), src, c1)
		c2 := a.advanceMain(t, repo.ID, src, c1)
		a.changeset(t, repo.ID, src, []corestore.RefUpdate{
			{Name: "refs/tags/v1", Expect: corestore.ExpectValue, New: corestore.Target(c2)},
		}, c2)
		cp := a.checkpoint(t, repo.ID, src)
		require.Equal(t, uint64(3), cp.Seq)
		require.Equal(t, 3, a.collectCovered(t, repo.ID, cp.Seq), "every pack up to the checkpoint is gone")

		// After the checkpoint: a move of main and the deletion of the tag.
		c3 := a.advanceMain(t, repo.ID, src, c2)
		a.changeset(t, repo.ID, src, []corestore.RefUpdate{
			{Name: "refs/tags/v1", Expect: corestore.ExpectValue, Old: corestore.Target(c2), New: ""},
		})

		b := a.secondNode(t, dialect)
		require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))
		repo, err := b.core.Meta.GetRepository(b.ctx, repo.Key)
		require.NoError(t, err)
		b.verifySynced(t, repo)

		refs, err := gitops.ReadRefs(b.ctx, b.localRepo())
		require.NoError(t, err)
		require.Equal(t, corestore.Target(c3), refs["refs/heads/main"])
		require.NotContains(t, refs, "refs/tags/v1", "a reference the replay deletes is not in the result")

		require.Equal(t, float64(1), counter(t, b.sy, "graft_storage_sync_checkpoint_seeds_total", ""))
		require.Equal(t, float64(2), counter(t, b.sy, "graft_storage_sync_changesets_total", ""), "only the changesets after the checkpoint")
		require.Equal(t, float64(1), syncCount(t, b.sy, "hydrated"))
	})
}

// TestSyncerSyncBoundary covers change C: a node a few changesets behind a new
// checkpoint catches up (case 7); a node below C_sync, the newest checkpoint
// at least gc_grace − sync_timeout − gc_margin old, hydrates from the
// checkpoint instead (case 8), because the packs it would catch up from may be
// collected.
func TestSyncerSyncBoundary(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		a := newEnv(t, envOptions{dialect: dialect})
		src := a.newSource(t)
		c1 := a.writeCommit(t, src)
		repo := a.register(t, a.key(localRelPath), src, c1)

		// B hydrates at 1, C at 2.
		b := a.secondNode(t, dialect)
		require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))
		c2 := a.advanceMain(t, repo.ID, src, c1)
		c := a.secondNode(t, dialect)
		require.NoError(t, c.sy.EnsureFresh(c.ctx, c.pbRepo(localRelPath)))

		c3 := a.advanceMain(t, repo.ID, src, c2)
		cp := a.checkpoint(t, repo.ID, src)
		require.Equal(t, uint64(3), cp.Seq)

		// Case 7: a new checkpoint is not C_sync yet, so B catches up to 3.
		require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))
		require.Equal(t, float64(1), syncCount(t, b.sy, "caught_up"))
		require.Equal(t, float64(1), syncCount(t, b.sy, "hydrated"))

		// The checkpoint becomes C_sync, and the head moves on.
		age := a.core.GCGrace - a.core.SyncTimeout - a.core.GCMargin
		a.ageCheckpoint(t, repo.ID, cp.Seq, age+time.Minute)
		a.advanceMain(t, repo.ID, src, c3)
		head, err := a.core.Meta.GetRepository(a.ctx, repo.Key)
		require.NoError(t, err)

		// Case 8: C, at 2, is below C_sync 3 and hydrates from the checkpoint.
		require.NoError(t, c.sy.EnsureFresh(c.ctx, c.pbRepo(localRelPath)))
		require.Equal(t, float64(2), syncCount(t, c.sy, "hydrated"))
		require.Zero(t, syncCount(t, c.sy, "caught_up"))
		require.Equal(t, float64(1), counter(t, c.sy, "graft_storage_sync_checkpoint_seeds_total", ""))
		c.verifySynced(t, head)

		// B, at 3, is not below C_sync 3: it catches up.
		require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))
		require.Equal(t, float64(2), syncCount(t, b.sy, "caught_up"))
		require.Equal(t, float64(1), syncCount(t, b.sy, "hydrated"))
		b.verifySynced(t, head)
	})
}

// blockingBlobs is a bucket whose reads last until the end of their context.
type blockingBlobs struct {
	corestore.BlobStore
	cancelled chan error
}

func (b blockingBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	<-ctx.Done()
	// The first read's cause is kept; a later read must not block the round.
	select {
	case b.cancelled <- ctx.Err():
	default:
	}
	return nil, ctx.Err()
}

// TestSyncerRoundBoundedBySyncTimeout: a sync round is bounded by sync_timeout,
// which cancels its bucket reads: a hydration whose download outlives it fails
// as Unavailable and its read is cancelled (GC BOUNDARY rule, enforcement).
//
// On a loaded machine the 200 ms budget can run out before the round starts a
// pack read (the round stops starting reads near its deadline, or a database
// call meets it first): the outcome is then the same, without a read. The
// round can also outlive the call, whose own deadline is set just before it.
// So the test waits for the round's end ("synced"), and checks the read's
// cancellation whenever a read started.
func TestSyncerRoundBoundedBySyncTimeout(t *testing.T) {
	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c1 := a.writeCommit(t, src)
	a.register(t, a.key(localRelPath), src, c1)

	b := a.secondNode(t, sqlstore.SQLite)
	cancelled := make(chan error, 1)
	b.core.Blobs = blockingBlobs{BlobStore: b.core.Blobs, cancelled: cancelled}
	b.core.SyncTimeout = 200 * time.Millisecond
	synced := make(chan struct{})
	b.sy.SetTestHook(once("synced", func() { close(synced) }))

	err := b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath))
	require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)

	// A deadlock detector, not a timing assumption: the round ends at its
	// 200 ms deadline.
	select {
	case <-synced:
	case <-time.After(time.Minute):
		t.Fatal("the sync round did not end")
	}
	select {
	case readErr := <-cancelled:
		require.ErrorIs(t, readErr, context.DeadlineExceeded, "the bucket read was cancelled by the round's deadline")
	default:
		t.Log("the round's budget ran out before it started a pack read")
	}

	_, ok, err := b.sy.Applied(b.ctx, b.pbRepo(localRelPath))
	require.NoError(t, err)
	require.False(t, ok, "nothing was installed")
}

// TestSyncerMissingCheckpointPack: a checkpoint whose pack is gone is
// corruption (the newest checkpoint is never collected): Internal naming the
// key, nothing installed.
func TestSyncerMissingCheckpointPack(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		a := newEnv(t, envOptions{dialect: dialect})
		src := a.newSource(t)
		c1 := a.writeCommit(t, src)
		repo := a.register(t, a.key(localRelPath), src, c1)
		cp := a.checkpoint(t, repo.ID, src)
		require.NoError(t, a.core.Blobs.Delete(a.ctx, cp.PackKey))

		b := a.secondNode(t, dialect)
		err := b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath))
		require.Equal(t, codes.Internal, status.Code(err), "%v", err)
		require.True(t, strings.Contains(err.Error(), cp.PackKey), fmt.Sprintf("the error names the pack: %v", err))
		_, ok, err := b.sy.Applied(b.ctx, b.pbRepo(localRelPath))
		require.NoError(t, err)
		require.False(t, ok)
	})
}
