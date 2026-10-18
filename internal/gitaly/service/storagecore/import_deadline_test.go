package storagecore

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The tests of this file check the write deadline of the import's pack
// (doc/graft/PLAN7.md, PACK OWNERSHIP RULE 4): an upload, or the commit of the
// changeset that references it, that would end after the deadline fails like
// a storage failure, no changeset references the pack, and the import is
// resumable as any failed import (IMPORT RULE).

const (
	slowNone  int32 = iota
	slowBlock       // Put waits for the end of its context and stores nothing
	slowLate        // Put stores the object, then returns only after the deadline
)

// slowBlobs is a bucket that is slower than the write deadline while its mode
// is not slowNone.
type slowBlobs struct {
	corestore.BlobStore
	mode *atomic.Int32
}

func (b slowBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	switch b.mode.Load() {
	case slowBlock:
		<-ctx.Done()
		return ctx.Err()
	case slowLate:
		if err := b.BlobStore.Put(ctx, key, r); err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	default:
		return b.BlobStore.Put(ctx, key, r)
	}
}

// testWriteDeadline is the write deadline of these tests: long enough for a
// normal upload and commit of the test repository, short enough to wait out.
// The core is never changed while the server runs: the RPCs order the test and
// the server only through a socket, which the race detector does not see.
const testWriteDeadline = 2 * time.Second

// setupSlow is setup with a slowBlobs bucket and a write deadline of w.
func setupSlow(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect, w time.Duration) (*harness, *atomic.Int32) {
	t.Helper()

	slow := &atomic.Int32{}
	h := setup(t, mode, dialect, func(core *corestore.Core) {
		core.Blobs = slowBlobs{BlobStore: core.Blobs, mode: slow}
		core.GCMargin = w
		core.GCGrace = 2 * w
	})
	return h, slow
}

// bucketKeys lists the objects of the repository in the bucket.
func (h *harness) bucketKeys(repoID int64) []string {
	h.t.Helper()

	var keys []string
	require.NoError(h.t, h.core.Blobs.List(h.ctx, fmt.Sprintf("repos/%d/", repoID), func(info corestore.BlobInfo) error {
		keys = append(keys, info.Key)
		return nil
	}))
	return keys
}

func TestImportRepositoryUploadPastTheWriteDeadline(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h, slow := setupSlow(t, mode, dialect, testWriteDeadline)
		h.seed()
		localBefore := h.localRefs()

		slow.Store(slowBlock)
		_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		requireResumeHint(t, err, codes.Unavailable)
		require.ErrorContains(t, err, corestore.ErrPackDeadline.Error())

		state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		repoID := state.GetRepositoryId()
		h.requireIncomplete(repoID)
		require.Empty(t, h.bucketKeys(repoID), "the cancelled upload stored nothing")

		slow.Store(slowNone)
		_, err = h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		h.requireImported(repoID, localBefore)
	})
}

func TestImportRepositoryCommitPastTheWriteDeadline(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h, slow := setupSlow(t, mode, dialect, testWriteDeadline)
		h.seed()
		localBefore := h.localRefs()

		// The upload completes, but only returns after the deadline: the changeset
		// that would reference the pack is not committed.
		slow.Store(slowLate)
		_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		requireResumeHint(t, err, codes.Unavailable)
		require.ErrorContains(t, err, corestore.ErrPackDeadline.Error())

		state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		repoID := state.GetRepositoryId()
		h.requireIncomplete(repoID)
		orphans := h.bucketKeys(repoID)
		require.Len(t, orphans, 1, "the pack was stored")

		// The resumed import uploads its own pack; the late one stays an orphan
		// that no changeset references.
		slow.Store(slowNone)
		_, err = h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		h.requireImported(repoID, localBefore)

		changesets, err := h.core.Meta.ListChangesets(h.ctx, repoID, 0, 1)
		require.NoError(t, err)
		require.NotEqual(t, orphans[0], changesets[0].PackKey)
		require.ElementsMatch(t, []string{orphans[0], changesets[0].PackKey}, h.bucketKeys(repoID))
	})
}
