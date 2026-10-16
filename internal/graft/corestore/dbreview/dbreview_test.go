package dbreview_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/dbreview"
	"github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// backend is one review store implementation under test. open returns two
// handles on the same reviews, as two nodes would have.
type backend struct {
	name string
	open func(t *testing.T) (store.ReviewStore, store.ReviewStore)
}

// backends returns the file store and the database store on every dialect.
// The conformance suite runs against all of them.
func backends() []backend {
	out := []backend{
		{
			name: "files",
			open: func(t *testing.T) (store.ReviewStore, store.ReviewStore) {
				dir := t.TempDir()
				return store.Open(dir), store.Open(dir)
			},
		},
	}

	for _, dialect := range coretest.Dialects() {
		out = append(out, backend{
			name: "database-" + string(dialect),
			open: func(t *testing.T) (store.ReviewStore, store.ReviewStore) {
				ctx := testhelper.Context(t)

				// Two cores on one database are two nodes.
				databaseURL := coretest.DatabaseURL(t, dialect)
				first := coretest.Open(t, coretest.Options{Dialect: dialect, DatabaseURL: databaseURL, NodeID: "node-a"})
				second := coretest.Open(t, coretest.Options{Dialect: dialect, DatabaseURL: databaseURL, NodeID: "node-b"})

				repo, _, err := first.Meta.RegisterRepository(ctx,
					corestore.RepoKey{Storage: "default", RelativePath: "repo.git"},
					"sha1", corestore.InitialState{})
				require.NoError(t, err)

				return dbreview.New(ctx, first.Meta, repo.ID), dbreview.New(ctx, second.Meta, repo.ID)
			},
		})
	}

	return out
}

// eachBackend runs fn as a subtest for every backend, with one handle.
func eachBackend(t *testing.T, fn func(t *testing.T, rs store.ReviewStore, other store.ReviewStore)) {
	t.Helper()
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			rs, other := b.open(t)
			fn(t, rs, other)
		})
	}
}

func ids(reviews []*gitalypb.Review) []string {
	out := make([]string, 0, len(reviews))
	for _, r := range reviews {
		out = append(out, r.GetId())
	}
	return out
}

func TestCreateGetList(t *testing.T) {
	eachBackend(t, func(t *testing.T, rs store.ReviewStore, _ store.ReviewStore) {
		a, err := rs.Create(&gitalypb.Review{TargetBranch: []byte("main"), Approved: true})
		require.NoError(t, err)
		require.Equal(t, "1", a.GetId())
		require.NotNil(t, a.GetCreatedAt())
		require.NotNil(t, a.GetUpdatedAt())
		require.False(t, a.GetApproved(), "approved is computed, not stored")
		require.Nil(t, a.GetRequirements())

		b, err := rs.Create(&gitalypb.Review{TargetBranch: []byte("dev")})
		require.NoError(t, err)
		require.Equal(t, "2", b.GetId())

		c, err := rs.Create(&gitalypb.Review{TargetBranch: []byte("main"), State: gitalypb.Review_STATE_LANDED})
		require.NoError(t, err)
		require.Equal(t, "3", c.GetId())

		got, err := rs.Get("1")
		require.NoError(t, err)
		require.Equal(t, []byte("main"), got.GetTargetBranch())
		require.False(t, got.GetApproved())

		for _, id := range []string{"99", "0", "", "x", "01"} {
			_, err := rs.Get(id)
			require.ErrorIs(t, err, store.ErrNotFound, "id %q", id)
		}

		all, err := rs.List("", false, 0)
		require.NoError(t, err)
		require.Equal(t, []string{"2", "1"}, ids(all), "landed review excluded, newest first")

		all, err = rs.List("", true, 0)
		require.NoError(t, err)
		require.Equal(t, []string{"3", "2", "1"}, ids(all))

		main, err := rs.List("main", true, 0)
		require.NoError(t, err)
		require.Equal(t, []string{"3", "1"}, ids(main))

		limited, err := rs.List("", true, 1)
		require.NoError(t, err)
		require.Equal(t, []string{"3"}, ids(limited))
	})
}

func TestUpdate(t *testing.T) {
	eachBackend(t, func(t *testing.T, rs store.ReviewStore, _ store.ReviewStore) {
		created, err := rs.Create(&gitalypb.Review{TargetBranch: []byte("main")})
		require.NoError(t, err)

		sentinel := errors.New("refused")
		_, err = rs.Update(created.GetId(), func(*gitalypb.Review) error { return sentinel })
		require.ErrorIs(t, err, sentinel, "the error of fn is returned unchanged")

		got, err := rs.Get(created.GetId())
		require.NoError(t, err)
		require.Equal(t, gitalypb.Review_STATE_OPEN_UNSPECIFIED, got.GetState(), "nothing was written")

		updated, err := rs.Update(created.GetId(), func(r *gitalypb.Review) error {
			r.State = gitalypb.Review_STATE_CLOSED
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, gitalypb.Review_STATE_CLOSED, updated.GetState())

		got, err = rs.Get(created.GetId())
		require.NoError(t, err)
		require.Equal(t, gitalypb.Review_STATE_CLOSED, got.GetState())
		require.False(t, got.GetUpdatedAt().AsTime().Before(created.GetCreatedAt().AsTime()))

		_, err = rs.Update("42", func(*gitalypb.Review) error { return nil })
		require.ErrorIs(t, err, store.ErrNotFound)
	})
}

func TestCreateTimestampsAreNow(t *testing.T) {
	eachBackend(t, func(t *testing.T, rs store.ReviewStore, _ store.ReviewStore) {
		before := time.Now().Add(-time.Second)
		r, err := rs.Create(&gitalypb.Review{TargetBranch: []byte("main")})
		require.NoError(t, err)
		require.True(t, r.GetCreatedAt().AsTime().After(before))
		require.True(t, r.GetCreatedAt().AsTime().Equal(r.GetUpdatedAt().AsTime()))
	})
}

// TestReviewsArePerRepository checks that two repositories of one database
// have their own review IDs and do not see each other's reviews.
func TestReviewsArePerRepository(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			ctx := testhelper.Context(t)
			core := coretest.Open(t, coretest.Options{Dialect: dialect})

			register := func(path string) store.ReviewStore {
				repo, _, err := core.Meta.RegisterRepository(ctx,
					corestore.RepoKey{Storage: "default", RelativePath: path}, "sha1", corestore.InitialState{})
				require.NoError(t, err)
				return dbreview.New(ctx, core.Meta, repo.ID)
			}
			a, b := register("a.git"), register("b.git")

			ra, err := a.Create(&gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("a")})
			require.NoError(t, err)
			rb, err := b.Create(&gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("b")})
			require.NoError(t, err)
			require.Equal(t, "1", ra.GetId())
			require.Equal(t, "1", rb.GetId())

			got, err := b.Get("1")
			require.NoError(t, err)
			require.Equal(t, []byte("b"), got.GetTitle())

			_, err = a.Create(&gitalypb.Review{TargetBranch: []byte("main")})
			require.NoError(t, err)

			listed, err := b.List("", true, 0)
			require.NoError(t, err)
			require.Equal(t, []string{"1"}, ids(listed))
		})
	}
}

// conflictingMeta is a MetaStore whose reviews always changed concurrently.
type conflictingMeta struct {
	corestore.MetaStore
	gets, updates int
	updateErr     error
}

func (m *conflictingMeta) GetReview(context.Context, int64, string) (*gitalypb.Review, int64, error) {
	m.gets++
	return &gitalypb.Review{Id: "1", TargetBranch: []byte("main")}, int64(m.gets), nil
}

func (m *conflictingMeta) UpdateReview(context.Context, int64, string, int64, *gitalypb.Review) error {
	m.updates++
	return m.updateErr
}

func TestUpdateGivesUpAfterTenConflicts(t *testing.T) {
	ctx := testhelper.Context(t)
	meta := &conflictingMeta{updateErr: corestore.ErrReviewVersionConflict}

	calls := 0
	_, err := dbreview.New(ctx, meta, 1).Update("1", func(*gitalypb.Review) error {
		calls++
		return nil
	})
	require.ErrorIs(t, err, corestore.ErrReviewVersionConflict)
	require.Equal(t, 10, calls, "fn runs once per attempt")
	require.Equal(t, 10, meta.gets, "every attempt reads the review again")
	require.Equal(t, 10, meta.updates)
}

func TestUpdateReturnsOtherErrorsAtOnce(t *testing.T) {
	ctx := testhelper.Context(t)
	broken := errors.New("database is down")
	meta := &conflictingMeta{updateErr: broken}

	_, err := dbreview.New(ctx, meta, 1).Update("1", func(*gitalypb.Review) error { return nil })
	require.ErrorIs(t, err, broken)
	require.Equal(t, 1, meta.updates)
}

func TestUpdateStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(testhelper.Context(t))
	meta := &conflictingMeta{updateErr: corestore.ErrReviewVersionConflict}

	_, err := dbreview.New(ctx, meta, 1).Update("1", func(*gitalypb.Review) error {
		cancel()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, meta.updates)
}

// TestConcurrentUpdatesLoseNothing runs updates through two handles at once.
// Every update appends one byte to the target branch, so the final length
// counts every update that was applied.
func TestConcurrentUpdatesLoseNothing(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			first, second := b.open(t)

			created, err := first.Create(&gitalypb.Review{TargetBranch: []byte("main")})
			require.NoError(t, err)

			const writers, updatesPerWriter = 2, 20
			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				errs []error
			)
			for _, rs := range []store.ReviewStore{first, second} {
				wg.Add(1)
				go func(rs store.ReviewStore) {
					defer wg.Done()
					for i := 0; i < updatesPerWriter; i++ {
						_, err := rs.Update(created.GetId(), func(r *gitalypb.Review) error {
							r.TargetBranch = append(r.TargetBranch, 'x')
							return nil
						})
						if err != nil {
							mu.Lock()
							errs = append(errs, err)
							mu.Unlock()
						}
					}
				}(rs)
			}
			wg.Wait()
			require.Empty(t, errs)

			got, err := first.Get(created.GetId())
			require.NoError(t, err)
			require.Len(t, got.GetTargetBranch(), len("main")+writers*updatesPerWriter)
		})
	}
}
