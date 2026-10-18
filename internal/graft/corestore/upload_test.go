package corestore

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeBlobs records the Puts; when block is set, Put waits for the end of its
// context instead of storing (a bucket slower than the write deadline).
type fakeBlobs struct {
	BlobStore
	block bool
	puts  []string
	// deadline is the deadline of the context of the last Put.
	deadline time.Time
}

func (f *fakeBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	f.puts = append(f.puts, key)
	f.deadline, _ = ctx.Deadline()
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	_, err := io.Copy(io.Discard, r)
	return err
}

func uploadCore(blobs BlobStore, grace, margin time.Duration) *Core {
	return &Core{Mode: ModeAuthoritative, Blobs: blobs, GCGrace: grace, GCMargin: margin}
}

func TestUploadPackUsesAnOwnedKeyAndTheWriteDeadline(t *testing.T) {
	t.Parallel()

	blobs := &fakeBlobs{}
	core := uploadCore(blobs, time.Hour, 5*time.Minute)
	sum := sha256.Sum256([]byte("pack"))
	readAt := time.Now()

	upload, err := core.UploadPack(context.Background(), 7, readAt, sum, 4, strings.NewReader("pack"))
	require.NoError(t, err)
	require.True(t, IsOwnedPackKey(upload.Key))
	id, gotSum, _ := ParsePackKey(upload.Key)
	require.Equal(t, int64(7), id)
	require.Equal(t, sum, gotSum)
	require.Equal(t, int64(4), upload.Bytes)
	require.Equal(t, readAt.Add(55*time.Minute), upload.Deadline)
	require.Equal(t, []string{upload.Key}, blobs.puts)
	require.WithinDuration(t, upload.Deadline, blobs.deadline, 0, "the Put runs under the write deadline")

	// Every upload gets its own key, also for the same bytes.
	again, err := core.UploadPack(context.Background(), 7, readAt, sum, 4, strings.NewReader("pack"))
	require.NoError(t, err)
	require.NotEqual(t, upload.Key, again.Key)

	ctx, cancel, err := upload.CommitContext(context.Background())
	require.NoError(t, err)
	defer cancel()
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	require.WithinDuration(t, upload.Deadline, deadline, 0, "the commit runs under the write deadline")
}

func TestUploadPackPastTheDeadlineStoresNothing(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("late"))

	t.Run("deadline passed before the upload", func(t *testing.T) {
		t.Parallel()
		blobs := &fakeBlobs{}
		core := uploadCore(blobs, time.Hour, 5*time.Minute)

		_, err := core.UploadPack(context.Background(), 1, time.Now().Add(-time.Hour), sum, 4, strings.NewReader("late"))
		require.ErrorIs(t, err, ErrPackDeadline)
		require.Empty(t, blobs.puts, "nothing is uploaded")
	})

	t.Run("deadline passes during the upload", func(t *testing.T) {
		t.Parallel()
		blobs := &fakeBlobs{block: true}
		core := uploadCore(blobs, 100*time.Millisecond, 50*time.Millisecond)

		_, err := core.UploadPack(context.Background(), 1, time.Now(), sum, 4, strings.NewReader("late"))
		require.ErrorIs(t, err, ErrPackDeadline, "the Put is cancelled at the deadline")
	})

	t.Run("caller cancels", func(t *testing.T) {
		t.Parallel()
		blobs := &fakeBlobs{block: true}
		core := uploadCore(blobs, time.Hour, 5*time.Minute)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := core.UploadPack(ctx, 1, time.Now(), sum, 4, strings.NewReader("late"))
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, ErrPackDeadline, "the caller's cancellation is not a deadline")
	})
}

func TestUploadPackRequiresTheGCTimes(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("x"))
	for _, core := range []*Core{
		uploadCore(&fakeBlobs{}, 0, 0),
		uploadCore(&fakeBlobs{}, time.Hour, 0),
		uploadCore(&fakeBlobs{}, time.Minute, time.Hour),
	} {
		_, err := core.UploadPack(context.Background(), 1, time.Now(), sum, 1, strings.NewReader("x"))
		require.ErrorContains(t, err, "invalid write deadline")
	}
}

func TestCommitContext(t *testing.T) {
	t.Parallel()

	expired := PackUpload{Key: "repos/1/packs/x.pack", Deadline: time.Now().Add(-time.Second)}
	_, _, err := expired.CommitContext(context.Background())
	require.True(t, errors.Is(err, ErrPackDeadline), "a commit past the deadline is not started")

	// A transaction without new objects uploaded nothing and has no deadline.
	ctx, cancel, err := PackUpload{}.CommitContext(context.Background())
	require.NoError(t, err)
	defer cancel()
	_, ok := ctx.Deadline()
	require.False(t, ok)
}
