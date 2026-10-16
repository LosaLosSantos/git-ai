package store

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func newReview(branch, title string) *gitalypb.Review {
	return &gitalypb.Review{
		TargetBranch: []byte(branch),
		BaseCommitId: "1111111111111111111111111111111111111111",
		HeadCommitId: "2222222222222222222222222222222222222222",
		Author:       "agent-1",
		Title:        []byte(title),
		State:        gitalypb.Review_STATE_OPEN_UNSPECIFIED,
		Requirements: []*gitalypb.ReviewRequirement{{Rule: "stale"}},
		Approved:     true,
		Files:        []*gitalypb.ReviewFile{{Path: []byte("a.go")}},
	}
}

func ids(reviews []*gitalypb.Review) []string {
	out := make([]string, 0, len(reviews))
	for _, r := range reviews {
		out = append(out, r.GetId())
	}
	return out
}

func TestSpecCreateAssignsIDsAndTimestamps(t *testing.T) {
	s := Open(t.TempDir())

	first, err := s.Create(newReview("main", "one"))
	require.NoError(t, err)
	second, err := s.Create(newReview("main", "two"))
	require.NoError(t, err)

	require.Equal(t, "1", first.GetId())
	require.Equal(t, "2", second.GetId())
	require.NotNil(t, first.GetCreatedAt())
	require.NotNil(t, first.GetUpdatedAt())
	require.Nil(t, first.GetRequirements(), "requirements are recomputed, not stored")
	require.False(t, first.GetApproved(), "approved is recomputed, not stored")
	require.Equal(t, []byte("a.go"), first.GetFiles()[0].GetPath())
}

func TestSpecCreateDoesNotMutateInput(t *testing.T) {
	s := Open(t.TempDir())
	in := newReview("main", "one")

	_, err := s.Create(in)
	require.NoError(t, err)

	require.Empty(t, in.GetId())
	require.Len(t, in.GetRequirements(), 1)
}

func TestSpecGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	created, err := Open(dir).Create(newReview("main", "one"))
	require.NoError(t, err)

	// A fresh Store on the same directory reads what the first one wrote.
	got, err := Open(dir).Get(created.GetId())
	require.NoError(t, err)
	require.Equal(t, created.GetId(), got.GetId())
	require.Equal(t, created.GetTitle(), got.GetTitle())
	require.Equal(t, created.GetHeadCommitId(), got.GetHeadCommitId())
	require.True(t, created.GetCreatedAt().AsTime().Equal(got.GetCreatedAt().AsTime()))
}

func TestSpecGetNotFound(t *testing.T) {
	s := Open(t.TempDir())

	_, err := s.Get("42")
	require.ErrorIs(t, err, ErrNotFound)

	for _, id := range []string{"", "0", "01", "../x", "1/2", "abc", "-1"} {
		_, err := s.Get(id)
		require.ErrorIs(t, err, ErrNotFound, "id %q", id)
	}
}

func TestSpecCorruptNextIDIsAnError(t *testing.T) {
	dir := t.TempDir()
	reviewsDir := filepath.Join(dir, "graft", "reviews")
	require.NoError(t, os.MkdirAll(reviewsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(reviewsDir, nextIDName), []byte("garbage\n"), 0o644))

	_, err := Open(dir).Create(newReview("main", "one"))
	require.Error(t, err)
}

func TestSpecListOrderFilterAndLimit(t *testing.T) {
	s := Open(t.TempDir())

	for _, branch := range []string{"main", "release", "main", "main"} {
		_, err := s.Create(newReview(branch, branch))
		require.NoError(t, err)
	}

	all, err := s.List("", false, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"4", "3", "2", "1"}, ids(all), "newest first")

	mainOnly, err := s.List("main", false, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"4", "3", "1"}, ids(mainOnly))

	limited, err := s.List("", false, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"4", "3"}, ids(limited))
}

func TestSpecListIncludeClosed(t *testing.T) {
	s := Open(t.TempDir())

	_, err := s.Create(newReview("main", "open"))
	require.NoError(t, err)
	_, err = s.Update("1", func(r *gitalypb.Review) error {
		r.State = gitalypb.Review_STATE_CLOSED
		return nil
	})
	require.NoError(t, err)
	_, err = s.Create(newReview("main", "still open"))
	require.NoError(t, err)

	open, err := s.List("", false, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"2"}, ids(open))

	withClosed, err := s.List("", true, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"2", "1"}, ids(withClosed))
}

func TestSpecListEmptyRepository(t *testing.T) {
	reviews, err := Open(t.TempDir()).List("", true, 0)
	require.NoError(t, err)
	require.Empty(t, reviews)
}

func TestSpecUpdateAppliesAndRefreshesUpdatedAt(t *testing.T) {
	s := Open(t.TempDir())
	created, err := s.Create(newReview("main", "one"))
	require.NoError(t, err)

	updated, err := s.Update(created.GetId(), func(r *gitalypb.Review) error {
		r.HeadCommitId = "3333333333333333333333333333333333333333"
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, "3333333333333333333333333333333333333333", updated.GetHeadCommitId())
	require.False(t, updated.GetUpdatedAt().AsTime().Before(created.GetUpdatedAt().AsTime()))

	got, err := s.Get(created.GetId())
	require.NoError(t, err)
	require.Equal(t, updated.GetHeadCommitId(), got.GetHeadCommitId())
}

func TestSpecUpdateErrorWritesNothing(t *testing.T) {
	s := Open(t.TempDir())
	created, err := s.Create(newReview("main", "one"))
	require.NoError(t, err)

	sentinel := errors.New("rejected")
	_, err = s.Update(created.GetId(), func(r *gitalypb.Review) error {
		r.Title = []byte("changed")
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	got, err := s.Get(created.GetId())
	require.NoError(t, err)
	require.Equal(t, []byte("one"), got.GetTitle())
}

func TestSpecUpdateNotFound(t *testing.T) {
	_, err := Open(t.TempDir()).Update("9", func(*gitalypb.Review) error { return nil })
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSpecConcurrentCreateUniqueIDs(t *testing.T) {
	s := Open(t.TempDir())

	const n = 20
	var wg sync.WaitGroup
	idsCh := make(chan string, n)
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Create(newReview("main", "concurrent"))
			if err != nil {
				errCh <- err
				return
			}
			idsCh <- r.GetId()
		}()
	}
	wg.Wait()
	close(idsCh)
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	seen := map[string]bool{}
	for id := range idsCh {
		require.False(t, seen[id], "duplicate id %s", id)
		seen[id] = true
	}
	require.Len(t, seen, n)

	all, err := s.List("", true, 0)
	require.NoError(t, err)
	require.Len(t, all, n)
}

func TestSpecConcurrentUpdatesAreNotLost(t *testing.T) {
	s := Open(t.TempDir())
	created, err := s.Create(newReview("main", "one"))
	require.NoError(t, err)

	const n = 20
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Update(created.GetId(), func(r *gitalypb.Review) error {
				r.Comments = append(r.Comments, &gitalypb.ReviewComment{Id: strconv.Itoa(len(r.GetComments()) + 1)})
				return nil
			})
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	got, err := s.Get(created.GetId())
	require.NoError(t, err)
	require.Len(t, got.GetComments(), n)
}

func TestSpecValidID(t *testing.T) {
	require.True(t, validID("1"))
	require.True(t, validID("1234567890"))
	require.False(t, validID(""))
	require.False(t, validID("0"))
	require.False(t, validID("007"))
	require.False(t, validID("1a"))
	require.False(t, validID("123456789012345678901"))
}
