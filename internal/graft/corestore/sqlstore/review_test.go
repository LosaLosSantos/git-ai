package sqlstore_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// insertReview inserts a review of the repository and returns its ID.
func insertReview(t *testing.T, ctx context.Context, s *sqlstore.Store, repoID int64, r *gitalypb.Review) *gitalypb.Review {
	t.Helper()

	inserted, err := s.InsertReview(ctx, repoID, func(id string) (*gitalypb.Review, error) {
		return r, nil
	}, false)
	require.NoError(t, err)
	return inserted
}

func TestReviewsCRUD(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("reviews"))

		first := insertReview(t, ctx, s, repo.ID, newReview("one", "main", gitalypb.Review_STATE_OPEN_UNSPECIFIED))
		second := insertReview(t, ctx, s, repo.ID, newReview("two", "release", gitalypb.Review_STATE_OPEN_UNSPECIFIED))
		third := insertReview(t, ctx, s, repo.ID, newReview("three", "main", gitalypb.Review_STATE_LANDED))
		require.Equal(t, "1", first.GetId())
		require.Equal(t, "2", second.GetId())
		require.Equal(t, "3", third.GetId())

		got, version, err := s.GetReview(ctx, repo.ID, "2")
		require.NoError(t, err)
		require.Equal(t, int64(1), version)
		require.Equal(t, "two", string(got.GetTitle()))
		require.Equal(t, "2", got.GetId())

		// Invalid and unknown IDs are not found.
		for _, id := range []string{"", "0", "01", "abc", "-1", "99"} {
			_, _, err := s.GetReview(ctx, repo.ID, id)
			require.ErrorIs(t, err, corestore.ErrReviewNotFound, id)
		}

		// Updates succeed on the current version only.
		updated := newReview("two, edited", "release", gitalypb.Review_STATE_OPEN_UNSPECIFIED)
		require.NoError(t, s.UpdateReview(ctx, repo.ID, "2", 1, updated, false))
		_, version, err = s.GetReview(ctx, repo.ID, "2")
		require.NoError(t, err)
		require.Equal(t, int64(2), version)

		err = s.UpdateReview(ctx, repo.ID, "2", 1, updated, false)
		require.ErrorIs(t, err, corestore.ErrReviewVersionConflict)

		err = s.UpdateReview(ctx, repo.ID, "99", 1, updated, false)
		require.ErrorIs(t, err, corestore.ErrReviewNotFound)
		err = s.UpdateReview(ctx, repo.ID, "abc", 1, updated, false)
		require.ErrorIs(t, err, corestore.ErrReviewNotFound)

		// Listing is newest first, filtered by target and by state.
		all, err := s.ListReviews(ctx, repo.ID, "", false, 0)
		require.NoError(t, err)
		require.Len(t, all, 2)
		require.Equal(t, "2", all[0].GetId())
		require.Equal(t, "1", all[1].GetId())

		withClosed, err := s.ListReviews(ctx, repo.ID, "", true, 0)
		require.NoError(t, err)
		require.Len(t, withClosed, 3)
		require.Equal(t, "3", withClosed[0].GetId())

		mainOnly, err := s.ListReviews(ctx, repo.ID, "main", true, 0)
		require.NoError(t, err)
		require.Len(t, mainOnly, 2)
		require.Equal(t, "3", mainOnly[0].GetId())
		require.Equal(t, "1", mainOnly[1].GetId())

		limited, err := s.ListReviews(ctx, repo.ID, "", true, 2)
		require.NoError(t, err)
		require.Len(t, limited, 2)
		require.Equal(t, "3", limited[0].GetId())
		require.Equal(t, "2", limited[1].GetId())
	})
}

func TestReviewsConcurrentUpdatesLoseNothing(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		dsn := coreDSN(t, s)
		repo := register(t, ctx, s, testKey("review-race"))
		insertReview(t, ctx, s, repo.ID, newReview("counter", "main", gitalypb.Review_STATE_OPEN_UNSPECIFIED))

		stores := []*sqlstore.Store{s, openStore(t, ctx, dsn)}
		const perStore = 20

		var wg sync.WaitGroup
		errs := make(chan error, len(stores))
		for _, store := range stores {
			wg.Add(1)
			go func(store *sqlstore.Store) {
				defer wg.Done()
				for i := 0; i < perStore; i++ {
					for {
						current, version, err := store.GetReview(ctx, repo.ID, "1")
						if err != nil {
							errs <- err
							return
						}

						next := fmt.Sprintf("%d", i)
						current.Title = []byte(next)
						err = store.UpdateReview(ctx, repo.ID, "1", version, current, false)
						if errors.Is(err, corestore.ErrReviewVersionConflict) {
							continue
						}
						if err != nil {
							errs <- err
							return
						}
						break
					}
				}
			}(store)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}

		_, version, err := s.GetReview(ctx, repo.ID, "1")
		require.NoError(t, err)
		require.Equal(t, int64(1+2*perStore), version)
	})
}

func TestCommitsOnOneReferenceAreDense(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		dsn := coreDSN(t, s)
		repo := register(t, ctx, s, testKey("contended"))
		_, err := commit(ctx, s, repo.ID, create("refs/heads/main", oid(1)))
		require.NoError(t, err)

		const goroutines, perGoroutine = 32, 20

		// Two stores on the same database behave like two nodes.
		stores := []*sqlstore.Store{s, openStore(t, ctx, dsn)}

		var wg sync.WaitGroup
		errs := make(chan error, goroutines)
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				store := stores[g%len(stores)]
				for i := 0; i < perGoroutine; i++ {
					newValue := oid(1000 + g*perGoroutine + i)
					for {
						refs, _, err := store.Refs(ctx, repo.ID)
						if err != nil {
							errs <- err
							return
						}

						_, err = commit(ctx, store, repo.ID, update("refs/heads/main", refs["refs/heads/main"], newValue))
						var conflict *corestore.ErrRefConflict
						if errors.As(err, &conflict) {
							continue
						}
						if err != nil {
							errs <- err
							return
						}
						break
					}
				}
			}(g)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}

		changesets, err := s.ListChangesets(ctx, repo.ID, 0, 1<<20)
		require.NoError(t, err)
		require.Len(t, changesets, 1+goroutines*perGoroutine)
		for i, cs := range changesets {
			require.Equal(t, uint64(i+1), cs.Seq)
		}

		updated, err := s.GetRepository(ctx, testKey("contended"))
		require.NoError(t, err)
		require.Equal(t, uint64(1+goroutines*perGoroutine), updated.HeadSeq)
	})
}

func TestCommitsOnDistinctReferencesNeverConflict(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("distinct"))

		const goroutines, perGoroutine = 32, 20

		var wg sync.WaitGroup
		errs := make(chan error, goroutines)
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				name := fmt.Sprintf("refs/heads/branch-%d", g)
				var current corestore.Target
				for i := 0; i < perGoroutine; i++ {
					next := oid(g*perGoroutine + i + 1)
					if _, err := commit(ctx, s, repo.ID, update(name, current, next)); err != nil {
						errs <- err
						return
					}
					current = next
				}
			}(g)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}

		changesets, err := s.ListChangesets(ctx, repo.ID, 0, 1<<20)
		require.NoError(t, err)
		require.Len(t, changesets, goroutines*perGoroutine)

		refs, head, err := s.Refs(ctx, repo.ID)
		require.NoError(t, err)
		require.Len(t, refs, goroutines)
		require.Equal(t, uint64(goroutines*perGoroutine), head)
	})
}

func TestTwoStoresBehaveLikeOne(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		dsn := coreDSN(t, s)
		other := openStore(t, ctx, dsn)

		repo := register(t, ctx, s, testKey("nodes"))
		_, err := commit(ctx, s, repo.ID, create("refs/heads/main", oid(1)))
		require.NoError(t, err)

		// The other node sees the write made by the first one.
		requireRefs(t, ctx, other, repo.ID, map[string]corestore.Target{"refs/heads/main": oid(1)})

		// A stale write from the other node conflicts, as it would on one node.
		_, err = commit(ctx, other, repo.ID, update("refs/heads/main", oid(9), oid(2)))
		var conflict *corestore.ErrRefConflict
		require.True(t, errors.As(err, &conflict))

		res, err := commit(ctx, other, repo.ID, update("refs/heads/main", oid(1), oid(2)))
		require.NoError(t, err)
		require.Equal(t, uint64(2), res.Seq)

		_, head, err := s.Refs(ctx, repo.ID)
		require.NoError(t, err)
		require.Equal(t, uint64(2), head)
	})
}
