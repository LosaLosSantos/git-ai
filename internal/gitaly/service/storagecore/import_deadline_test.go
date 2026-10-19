package storagecore

import (
	"context"
	"fmt"
	"io"
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
// changeset that references it, that would end after the deadline fails like a
// storage failure and no changeset references the pack. The import is then
// incomplete (head sequence 0), which the IMPORT RULE tests show is resumed by
// the next ImportRepository; they run with the production deadline, while these
// tests use a short one that the server must keep for its whole life.

// slowMode is how a slowBlobs bucket misses the write deadline.
type slowMode int

const (
	// slowBlock: Put waits for the end of its context and stores nothing.
	slowBlock slowMode = iota
	// slowLate: Put stores the object, whatever the deadline, and returns only
	// once the deadline has passed.
	slowLate
)

// slowBlobs is a bucket slower than the write deadline.
type slowBlobs struct {
	corestore.BlobStore
	mode slowMode
}

func (b slowBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	if b.mode == slowBlock {
		<-ctx.Done()
		return ctx.Err()
	}

	if err := b.BlobStore.Put(context.WithoutCancel(ctx), key, r); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// testWriteDeadline is the write deadline of these tests, short enough to
// wait out. Nothing in them needs a normal upload to fit into it.
const testWriteDeadline = 2 * time.Second

// setupSlow is setup with a slowBlobs bucket in mode and the write deadline
// testWriteDeadline.
func setupSlow(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect, slow slowMode) *harness {
	t.Helper()

	return setup(t, mode, dialect, func(core *corestore.Core) {
		core.Blobs = slowBlobs{BlobStore: core.Blobs, mode: slow}
		core.GCMargin = testWriteDeadline
		core.GCGrace = 2 * testWriteDeadline
	})
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

// failedImport runs an import that misses the write deadline and returns the
// ID of its registration, which is left incomplete.
func (h *harness) failedImport() int64 {
	h.t.Helper()

	_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
	requireResumeHint(h.t, err, codes.Unavailable)
	require.ErrorContains(h.t, err, corestore.ErrPackDeadline.Error())

	state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
	require.NoError(h.t, err)
	repoID := state.GetRepositoryId()
	h.requireIncomplete(repoID)
	return repoID
}

func TestImportRepositoryUploadPastTheWriteDeadline(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setupSlow(t, mode, dialect, slowBlock)
		h.seed()

		repoID := h.failedImport()
		require.Empty(t, h.bucketKeys(repoID), "the cancelled upload stored nothing")
	})
}

func TestImportRepositoryCommitPastTheWriteDeadline(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setupSlow(t, mode, dialect, slowLate)
		h.seed()

		// The upload completes, but returns only after the deadline: the
		// changeset that would reference the pack is not committed, and the pack
		// is an orphan that no changeset references.
		repoID := h.failedImport()
		require.Len(t, h.bucketKeys(repoID), 1, "the pack was stored")
	})
}
