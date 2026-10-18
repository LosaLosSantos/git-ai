package voter_test

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
)

// setWriteDeadline gives the fixture's core the write deadline w
// (doc/graft/PLAN7.md, PACK OWNERSHIP RULE 4: W = gc_grace - gc_margin).
func (f *fixture) setWriteDeadline(w time.Duration) {
	f.core.GCMargin = w
	f.core.GCGrace = 2 * w
}

// blockingBlobs is a bucket slower than any write deadline: every Put waits for
// the end of its context and stores nothing.
type blockingBlobs struct {
	corestore.BlobStore
}

func (blockingBlobs) Put(ctx context.Context, _ string, _ io.Reader) error {
	<-ctx.Done()
	return ctx.Err()
}

// packKeys lists the objects of the repository in the bucket.
func (f *fixture) packKeys(t *testing.T, blobs corestore.BlobStore) []string {
	t.Helper()

	var keys []string
	require.NoError(t, blobs.List(f.ctx, fmt.Sprintf("repos/%d/", f.repoID), func(info corestore.BlobInfo) error {
		keys = append(keys, info.Key)
		return nil
	}))
	return keys
}

// TestPreparedCommitsOnlyBeforeTheWriteDeadline checks that a changeset whose
// pack was uploaded is not committed once the pack's write deadline passed: the
// write fails like a storage failure and no row references the pack, which
// garbage collection may already treat as an orphan.
func TestPreparedCommitsOnlyBeforeTheWriteDeadline(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, mode := range []corestore.Mode{corestore.ModeAuthoritative, corestore.ModeShadow} {
			t.Run(string(mode), func(t *testing.T) {
				f := newFixture(t, fixtureOptions{mode: mode, dialect: dialect})
				f.setWriteDeadline(time.Second)
				blobs := f.countPuts()
				before := f.headSeq(t)

				// The work between the upload and the commit outlasts the deadline,
				// which started before the upload.
				voter.FaultHook = func(point string) error {
					if point == "after_upload" {
						<-time.After(f.core.WriteDeadline())
					}
					return nil
				}
				t.Cleanup(func() { voter.FaultHook = nil })

				err := f.prepared(line(f.zero, f.c.String(), ref))
				if mode == corestore.ModeAuthoritative {
					require.Equal(t, codes.Unavailable, status.Code(err), "error %v", err)
					require.ErrorIs(t, err, corestore.ErrPackDeadline)
				} else {
					require.NoError(t, err, "shadow mode never fails the write")
					require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
				}

				require.Equal(t, 1, blobs.count(), "the pack was uploaded")
				require.Equal(t, before, f.headSeq(t), "no changeset references it")
			})
		}
	})
}

// TestPreparedUploadsAgainWhenThePreparedPackExpired checks that the pack of
// the Preparing phase is not committed when its write deadline passed before
// the Prepared phase (the transaction waited long for the reference lock): the
// Prepared phase uploads a new pack under a new key and commits that one.
func TestPreparedUploadsAgainWhenThePreparedPackExpired(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
		f.setWriteDeadline(time.Second)
		blobs := f.countPuts()

		input := line(f.zero, f.c.String(), ref)
		f.preparing(input)
		require.Equal(t, 1, blobs.count())
		require.Equal(t, 1, f.voter.PreparedPacks())

		<-time.After(f.core.WriteDeadline())
		// A long deadline again, so that only the expiry of the prepared pack is
		// under test.
		f.setWriteDeadline(time.Hour)

		require.NoError(t, f.prepared(input))
		require.Equal(t, 2, blobs.count(), "an expired pack is never committed")
		require.NotEqual(t, blobs.puts[0], blobs.puts[1])
		require.Equal(t, blobs.puts[1], f.lastChangeset(t).PackKey)
		require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref))
	})
}

// TestUploadIsCancelledAtTheWriteDeadline checks that an upload slower than
// the write deadline is cancelled, stores nothing and fails the write; no
// changeset is committed.
func TestUploadIsCancelledAtTheWriteDeadline(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
		f.setWriteDeadline(100 * time.Millisecond)
		inner := f.core.Blobs
		keysBefore := f.packKeys(t, inner)
		f.core.Blobs = blockingBlobs{BlobStore: inner}
		before := f.headSeq(t)

		input := line(f.zero, f.c.String(), ref)
		f.preparing(input)
		err := f.prepared(input)
		require.Equal(t, codes.Unavailable, status.Code(err), "error %v", err)
		require.ErrorIs(t, err, corestore.ErrPackDeadline)

		require.Equal(t, before, f.headSeq(t), "no changeset was committed")
		require.Equal(t, keysBefore, f.packKeys(t, inner), "nothing was stored")
	})
}
