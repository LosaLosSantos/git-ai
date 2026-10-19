package storagecore

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// shortGC makes garbage collection wait a short grace, so that a test sees an
// orphan deleted. The writers' deadline (grace - margin) stays long enough for
// the import's upload under load.
func shortGC(core *corestore.Core) {
	core.GCGrace = time.Second
	core.GCMargin = 100 * time.Millisecond
}

// putOrphan stores an object under the repository's pack prefix that no row
// references, as a write that died before its database commit leaves.
func (h *harness) putOrphan(repoID int64, content string) string {
	h.t.Helper()
	key := corestore.NewPackKey(repoID, sha256.Sum256([]byte(content)))
	require.NoError(h.t, h.core.Blobs.Put(h.ctx, key, bytes.NewReader([]byte(content))))
	return key
}

func (h *harness) exists(key string) bool {
	h.t.Helper()
	ok, err := h.core.Blobs.Exists(h.ctx, key)
	require.NoError(h.t, err)
	return ok
}

func TestCreateCheckpoint(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeAuthoritative, dialect)
		h.seed()
		repoID := h.importRepo()

		resp, err := h.client.CreateCheckpoint(h.ctx, &gitalypb.CreateCheckpointRequest{Repository: h.repo})
		require.NoError(t, err)
		record, err := h.core.Meta.GetRepository(h.ctx, corestore.KeyOf(h.repo))
		require.NoError(t, err)
		require.Equal(t, repoID, record.ID)
		require.True(t, resp.GetCreated())
		require.Equal(t, record.HeadSeq, resp.GetCheckpointSequence())
		require.Positive(t, resp.GetPackBytes())
		require.Equal(t, uint32(len(h.localRefs())), resp.GetReferences(), "HEAD, the branch and the tag")

		again, err := h.client.CreateCheckpoint(h.ctx, &gitalypb.CreateCheckpointRequest{Repository: h.repo})
		require.NoError(t, err)
		require.False(t, again.GetCreated(), "a checkpoint at the head exists")
		require.Equal(t, resp.GetCheckpointSequence(), again.GetCheckpointSequence())
	})
}

func TestCreateCheckpointRefusals(t *testing.T) {
	t.Parallel()

	off := setup(t, corestore.ModeOff, sqlstore.SQLite)
	_, err := off.client.CreateCheckpoint(off.ctx, &gitalypb.CreateCheckpointRequest{Repository: off.repo})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)

	shadow := setup(t, corestore.ModeShadow, sqlstore.SQLite)
	shadow.seed()
	shadow.importRepo()
	_, err = shadow.client.CreateCheckpoint(shadow.ctx, &gitalypb.CreateCheckpointRequest{Repository: shadow.repo})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "checkpoints are authoritative mode only: %v", err)

	untracked := setup(t, corestore.ModeAuthoritative, sqlstore.SQLite)
	_, err = untracked.client.CreateCheckpoint(untracked.ctx, &gitalypb.CreateCheckpointRequest{Repository: untracked.repo})
	require.Equal(t, codes.NotFound, status.Code(err), "%v", err)

	_, err = untracked.client.CreateCheckpoint(untracked.ctx, &gitalypb.CreateCheckpointRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
}

func TestCollectGarbage(t *testing.T) {
	t.Parallel()
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect, shortGC)
		h.seed()
		repoID := h.importRepo()
		orphan := h.putOrphan(repoID, "the pack of a write that died")

		dry, err := h.client.CollectGarbage(h.ctx, &gitalypb.CollectGarbageRequest{Repository: h.repo, DryRun: true})
		require.NoError(t, err)
		require.True(t, dry.GetDryRun())
		require.Equal(t, uint32(0), dry.GetPacksDeleted())
		require.True(t, h.exists(orphan))

		first, err := h.client.CollectGarbage(h.ctx, &gitalypb.CollectGarbageRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Equal(t, uint32(1), first.GetOrphansObserved())
		require.True(t, h.exists(orphan), "observed, not yet deleted")

		require.Eventually(t, func() bool {
			resp, err := h.client.CollectGarbage(h.ctx, &gitalypb.CollectGarbageRequest{Repository: h.repo})
			require.NoError(t, err)
			return resp.GetOrphansDeleted() == 1 && resp.GetBytesReclaimed() > 0
		}, 30*time.Second, 50*time.Millisecond)
		require.False(t, h.exists(orphan))
		require.Empty(t, h.verify(true).GetDivergences(), "the repository is intact")
	})
}

func TestCollectGarbageRefusals(t *testing.T) {
	t.Parallel()

	off := setup(t, corestore.ModeOff, sqlstore.SQLite)
	_, err := off.client.CollectGarbage(off.ctx, &gitalypb.CollectGarbageRequest{Repository: off.repo})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)

	h := setup(t, corestore.ModeAuthoritative, sqlstore.SQLite)
	_, err = h.client.CollectGarbage(h.ctx, &gitalypb.CollectGarbageRequest{Repository: h.repo})
	require.Equal(t, codes.NotFound, status.Code(err), "untracked: %v", err)

	h.seed()
	repoID := h.importRepo()
	acquired, err := h.core.Meta.AcquireMaintenanceLease(h.ctx, repoID, "node-b/1", time.Hour)
	require.NoError(t, err)
	require.True(t, acquired)
	_, err = h.client.CollectGarbage(h.ctx, &gitalypb.CollectGarbageRequest{Repository: h.repo})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
	require.Contains(t, err.Error(), "another maintenance operation of this repository is in progress")

	require.NoError(t, h.core.Meta.ReleaseMaintenanceLease(h.ctx, repoID, "node-b/1"))
	_, err = h.client.CollectGarbage(h.ctx, &gitalypb.CollectGarbageRequest{Repository: h.repo})
	require.NoError(t, err)
	// The request released its lease.
	acquired, err = h.core.Meta.AcquireMaintenanceLease(h.ctx, repoID, "node-b/1", time.Hour)
	require.NoError(t, err)
	require.True(t, acquired)
}

func TestCollectRemovedRepositories(t *testing.T) {
	t.Parallel()
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect, shortGC)
		h.seed()
		repoID := h.importRepo()
		packs := h.packKeys(repoID)
		require.NotEmpty(t, packs)

		removed, err := h.core.Meta.DeleteRepository(h.ctx, corestore.KeyOf(h.repo))
		require.NoError(t, err)
		require.True(t, removed)

		storageName := h.cfg.Storages[0].Name
		require.Eventually(t, func() bool {
			resp, err := h.client.CollectRemovedRepositories(h.ctx, &gitalypb.CollectRemovedRepositoriesRequest{StorageName: storageName})
			require.NoError(t, err)
			return resp.GetRepositories() == 1 && resp.GetPacksDeleted() >= uint32(len(packs))
		}, 30*time.Second, 50*time.Millisecond)
		require.Empty(t, h.packKeys(repoID))
	})
}

func TestCollectRemovedRepositoriesRefusals(t *testing.T) {
	t.Parallel()

	off := setup(t, corestore.ModeOff, sqlstore.SQLite)
	_, err := off.client.CollectRemovedRepositories(off.ctx, &gitalypb.CollectRemovedRepositoriesRequest{StorageName: off.cfg.Storages[0].Name})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)

	h := setup(t, corestore.ModeAuthoritative, sqlstore.SQLite)
	_, err = h.client.CollectRemovedRepositories(h.ctx, &gitalypb.CollectRemovedRepositoriesRequest{StorageName: "no-such-storage"})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
}

// TestStorageCoreRejectsReservedRelativePaths verifies that no storage core
// RPC acts on a repository inside Gitaly's data directory, where the storage
// core keeps untrusted copies for an operator: the gate does not see these
// RPCs, so they check it themselves.
func TestStorageCoreRejectsReservedRelativePaths(t *testing.T) {
	t.Parallel()

	h := setup(t, corestore.ModeAuthoritative, sqlstore.SQLite)
	repo := &gitalypb.Repository{StorageName: h.repo.GetStorageName(), RelativePath: "+gitaly/graft-untrusted/stale-1/repo.git"}
	for name, call := range map[string]func() error{
		"GetStorageState": func() error {
			_, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: repo})
			return err
		},
		"VerifyRepository": func() error {
			_, err := h.client.VerifyRepository(h.ctx, &gitalypb.VerifyRepositoryRequest{Repository: repo})
			return err
		},
		"CreateCheckpoint": func() error {
			_, err := h.client.CreateCheckpoint(h.ctx, &gitalypb.CreateCheckpointRequest{Repository: repo})
			return err
		},
		"CollectGarbage": func() error {
			_, err := h.client.CollectGarbage(h.ctx, &gitalypb.CollectGarbageRequest{Repository: repo})
			return err
		},
	} {
		err := call()
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", name, err)
		require.ErrorContains(t, err, "Gitaly's data directory", name)
	}
}

// packKeys returns the objects of the repository in the bucket.
func (h *harness) packKeys(repoID int64) map[string]bool {
	h.t.Helper()
	keys := map[string]bool{}
	require.NoError(h.t, h.core.Blobs.List(h.ctx, fmt.Sprintf("repos/%d/", repoID), func(info corestore.BlobInfo) error {
		keys[info.Key] = true
		return nil
	}))
	return keys
}
