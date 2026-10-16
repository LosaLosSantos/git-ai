package sdk_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

func changesetLog(n int) []*gitalypb.Changeset {
	log := make([]*gitalypb.Changeset, 0, n)
	for i := 1; i <= n; i++ {
		log = append(log, &gitalypb.Changeset{
			Sequence:  uint64(i),
			Reference: []byte("refs/heads/main"),
			NewOid:    "oid",
			Operation: "land",
			Actor:     "agent-1",
		})
	}
	return log
}

func TestRepo_Changesets(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.changesets.log = changesetLog(5)
	repo := dialFake(t, fakes.register).Repo("repo")

	changesets, err := repo.Changesets(t.Context(), 2, 2)
	require.NoError(t, err)
	require.Len(t, changesets, 2)
	require.Equal(t, uint64(3), changesets[0].GetSequence())
	require.Equal(t, uint64(4), changesets[1].GetSequence())
	require.Equal(t, "land", changesets[0].GetOperation())
	require.Equal(t, "agent-1", changesets[0].GetActor())

	all, err := repo.Changesets(t.Context(), 0, 0)
	require.NoError(t, err)
	require.Len(t, all, 5)
}

func TestRepo_WatchChangesets_BacklogThenLive(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.changesets.log = changesetLog(3)
	fakes.changesets.live = make(chan *gitalypb.Changeset)
	repo := dialFake(t, fakes.register).Repo("repo")

	ctx, cancel := context.WithCancel(t.Context())
	changes, errs := repo.WatchChangesets(ctx, 1)

	require.Equal(t, uint64(2), receiveChangeset(t, changes).GetSequence())
	require.Equal(t, uint64(3), receiveChangeset(t, changes).GetSequence())

	fakes.changesets.live <- &gitalypb.Changeset{Sequence: 4, Operation: "commit_files"}
	live := receiveChangeset(t, changes)
	require.Equal(t, uint64(4), live.GetSequence())
	require.Equal(t, "commit_files", live.GetOperation())

	cancel()
	requireClosedWithoutError(t, changes, errs)
}

func TestRepo_WatchChangesets_ResumesAfterTransientFailure(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.changesets.log = changesetLog(3)
	fakes.changesets.live = make(chan *gitalypb.Changeset)
	fakes.changesets.dropAfter = 1
	repo := dialFake(t, fakes.register).Repo("repo")

	ctx, cancel := context.WithCancel(t.Context())
	changes, errs := repo.WatchChangesets(ctx, 0)

	// Sequence 1 arrives, the stream drops, and the watch reconnects after 1
	// without repeating or skipping changesets.
	require.Equal(t, uint64(1), receiveChangeset(t, changes).GetSequence())
	require.Equal(t, uint64(2), receiveChangeset(t, changes).GetSequence())
	require.Equal(t, uint64(3), receiveChangeset(t, changes).GetSequence())

	fakes.changesets.mu.Lock()
	requests := fakes.changesets.watchRequests
	fakes.changesets.mu.Unlock()
	require.GreaterOrEqual(t, len(requests), 2)
	require.Equal(t, uint64(0), requests[0].GetAfterSequence())
	require.Equal(t, uint64(1), requests[1].GetAfterSequence())

	cancel()
	requireClosedWithoutError(t, changes, errs)
}

func TestRepo_WatchChangesets_PermanentErrorIsReported(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	fakes.changesets.watchErr = status.Error(codes.NotFound, "repository not found")
	repo := dialFake(t, fakes.register).Repo("repo")

	changes, errs := repo.WatchChangesets(t.Context(), 0)

	select {
	case err := <-errs:
		require.True(t, sdk.IsNotFound(err))
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the watch error")
	}

	_, ok := <-changes
	require.False(t, ok, "changes channel must be closed after a permanent error")
	require.Len(t, fakes.changesets.watchRequests, 1, "permanent errors are not retried")
}

func receiveChangeset(t *testing.T, changes <-chan *sdk.Changeset) *sdk.Changeset {
	t.Helper()

	select {
	case cs, ok := <-changes:
		require.True(t, ok, "changes channel closed unexpectedly")
		return cs
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a changeset")
		return nil
	}
}

func requireClosedWithoutError(t *testing.T, changes <-chan *sdk.Changeset, errs <-chan error) {
	t.Helper()

	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-changes:
			if !ok {
				select {
				case err, ok := <-errs:
					require.False(t, ok, "unexpected error: %v", err)
				case <-timeout:
					t.Fatal("errs channel was not closed")
				}
				return
			}
		case <-timeout:
			t.Fatal("changes channel was not closed")
		}
	}
}
