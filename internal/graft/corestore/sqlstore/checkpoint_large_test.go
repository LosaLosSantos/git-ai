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

// TestCheckpointOfManyReferences records the snapshot of a repository with
// 100,000 references (the batched inserts of CreateCheckpoint) and reads it back
// whole and in order, while a writer commits changesets of the same repository
// back to back. It logs how long the record took and the longest changeset
// commit during it: the cost writers pay for a checkpoint of a large
// repository (integrated review M4). The record does not lock the repository
// row, so on Postgres the writer does not wait for it; SQLite has one write
// lock for the whole database, so there the writer waits for the record's
// transaction.
func TestCheckpointOfManyReferences(t *testing.T) {
	const references = 100_000

	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo, _ := headRepo(t, ctx, s, "many references", 1)
		refs := make(map[string]corestore.Target, references)
		refs["HEAD"] = corestore.SymbolicTarget("refs/heads/main")
		for i := 1; i < references; i++ {
			refs[fmt.Sprintf("refs/heads/branch-%06d", i)] = oid(i)
		}

		done := make(chan struct{})
		type writes struct {
			n       int
			longest time.Duration
			err     error
		}
		written := make(chan writes, 1)
		go func() {
			var w writes
			defer func() { written <- w }()
			for prev := oid(1); ; w.n++ {
				select {
				case <-done:
					return
				default:
				}
				next := oid(2 + w.n)
				began := time.Now()
				if _, w.err = commitPack(ctx, s, repo.ID, update("refs/heads/main", prev, next), ownedKey(repo.ID, fmt.Sprintf("write %d", w.n))); w.err != nil {
					return
				}
				w.longest = max(w.longest, time.Since(began))
				prev = next
			}
		}()

		began := time.Now()
		_, created, err := s.CreateCheckpoint(ctx, repo.ID, corestore.CheckpointInput{
			Seq: 1, Refs: refs, PackKey: ownedKey(repo.ID, "large checkpoint"), PackBytes: 1,
		})
		took := time.Since(began)
		close(done)
		require.NoError(t, err)
		require.True(t, created)
		w := <-written
		require.NoError(t, w.err)
		t.Logf("%s: checkpoint of %d references recorded in %s; %d changesets committed meanwhile, the longest in %s",
			s.Dialect(), references, took, w.n, w.longest)

		got, err := s.CheckpointRefs(ctx, repo.ID, 1)
		require.NoError(t, err)
		require.Equal(t, refs, got)
	})
}
