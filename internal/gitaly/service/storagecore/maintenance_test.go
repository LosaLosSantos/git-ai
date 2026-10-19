package storagecore

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The tests age rows on the database clock instead of waiting with a short
// gc_grace: the writers' deadline W is gc_grace - gc_margin, and an import or
// a shadow write under the race detector on a loaded host does not fit in a W
// of a second.

// exec runs a statement with "?" placeholders on the database.
func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	store := h.core.Meta.(*sqlstore.Store)
	if store.Dialect() == sqlstore.Postgres {
		for n := 1; strings.Contains(query, "?"); n++ {
			query = strings.Replace(query, "?", fmt.Sprintf("$%d", n), 1)
		}
	}
	_, err := store.DB().ExecContext(h.ctx, query, args...)
	require.NoError(h.t, err)
}

// longAgo returns the database time value of more than gc_grace before now.
func (h *harness) longAgo() any {
	t := time.Now().Add(-h.core.GCGrace - time.Minute).UTC()
	if h.core.Meta.(*sqlstore.Store).Dialect() == sqlstore.SQLite {
		return t.Format(time.RFC3339Nano)
	}
	return t
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
		main := h.seed()
		repoID := h.importRepo()

		resp, err := h.client.CreateCheckpoint(h.ctx, &gitalypb.CreateCheckpointRequest{Repository: h.repo})
		require.NoError(t, err)
		record, err := h.core.Meta.GetRepository(h.ctx, corestore.KeyOf(h.repo))
		require.NoError(t, err)
		require.Equal(t, repoID, record.ID)
		require.True(t, resp.GetCreated())
		require.Equal(t, record.HeadSeq, resp.GetCheckpointSequence())
		require.Positive(t, resp.GetPackBytes())
		// The open review imported from the files has no keep-around reference:
		// the build backfills it through the write path first (integrated review
		// m3), so the checkpoint holds HEAD, the branch, the tag and the review
		// root, as the database and the local copy do.
		snapshot, err := h.core.Meta.CheckpointRefs(h.ctx, repoID, resp.GetCheckpointSequence())
		require.NoError(t, err)
		require.Equal(t, corestore.Target(main), snapshot[corestore.KeepAroundRef(main.String())])
		require.Equal(t, h.localRefs(), snapshot)
		require.Equal(t, uint32(4), resp.GetReferences())

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
		h := setup(t, mode, dialect)
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

		h.exec("UPDATE graft_gc_orphans SET first_seen = ? WHERE repo_id = ?", h.longAgo(), repoID)
		resp, err := h.client.CollectGarbage(h.ctx, &gitalypb.CollectGarbageRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Equal(t, uint32(1), resp.GetOrphansDeleted())
		require.Positive(t, resp.GetBytesReclaimed())
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
		h := setup(t, mode, dialect)
		h.seed()
		repoID := h.importRepo()
		packs := h.packKeys(repoID)
		require.NotEmpty(t, packs)

		removed, err := h.core.Meta.DeleteRepository(h.ctx, corestore.KeyOf(h.repo))
		require.NoError(t, err)
		require.True(t, removed)

		storageName := h.cfg.Storages[0].Name
		collect := func() *gitalypb.CollectRemovedRepositoriesResponse {
			resp, err := h.client.CollectRemovedRepositories(h.ctx, &gitalypb.CollectRemovedRepositoriesRequest{StorageName: storageName})
			require.NoError(t, err)
			return resp
		}
		require.Zero(t, collect().GetRepositories(), "the tombstone is younger than gc_grace")
		h.exec("UPDATE graft_gc_removed SET removed_at = ? WHERE repo_id = ?", h.longAgo(), repoID)
		resp := collect()
		require.Equal(t, uint32(1), resp.GetRepositories())
		require.GreaterOrEqual(t, resp.GetPacksDeleted(), uint32(len(packs)))
		require.Empty(t, h.packKeys(repoID))
	})
}

// TestCollectRemovedRepositoriesReconcilesLeaks: the objects of a repository
// whose row went without a tombstone (deleted by hand before the trigger
// existed) are found by the reconciliation of the RPC once the allocation is W
// old, get a tombstone, and are deleted once it is gc_grace old (integrated
// review m4).
func TestCollectRemovedRepositoriesReconcilesLeaks(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeAuthoritative, dialect)
		h.seed()
		repoID := h.importRepo()
		packs := h.packKeys(repoID)
		require.NotEmpty(t, packs)

		h.exec("DELETE FROM graft_repositories WHERE repo_id = ?", repoID)
		h.exec("DELETE FROM graft_gc_removed WHERE repo_id = ?", repoID)

		storageName := h.cfg.Storages[0].Name
		collect := func() *gitalypb.CollectRemovedRepositoriesResponse {
			resp, err := h.client.CollectRemovedRepositories(h.ctx, &gitalypb.CollectRemovedRepositoriesRequest{StorageName: storageName})
			require.NoError(t, err)
			require.Zero(t, resp.GetIdsAheadOfAllocator())
			return resp
		}
		require.Zero(t, collect().GetLeaksFound(), "the allocation is not W old yet")

		// The allocator was observed at the ID longer than W ago.
		h.exec("DELETE FROM graft_gc_allocator_marks")
		h.exec("INSERT INTO graft_gc_allocator_marks (observed_at, position) VALUES (?, ?)", h.longAgo(), repoID)
		resp := collect()
		require.Equal(t, uint32(1), resp.GetLeaksFound())
		require.Equal(t, uint32(1), resp.GetLeaksTombstoned())
		require.Zero(t, resp.GetRepositories(), "the new tombstone is younger than gc_grace")

		h.exec("UPDATE graft_gc_removed SET removed_at = ? WHERE repo_id = ?", h.longAgo(), repoID)
		resp = collect()
		require.Equal(t, uint32(1), resp.GetRepositories())
		require.Zero(t, resp.GetLeaksFound(), "examined once")
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

// TestImportRefusesAnUntrustedRepository verifies that ImportRepository, which
// the gate does not see, never makes a local copy whose references may have
// lost writes the database's truth.
func TestImportRefusesAnUntrustedRepository(t *testing.T) {
	t.Parallel()

	h := setup(t, corestore.ModeAuthoritative, sqlstore.SQLite)
	ref := localrefs.RepoRef{Storage: h.repo.GetStorageName(), RelativePath: h.repo.GetRelativePath()}

	// A marker that records a failed fsync, as a durable step of the previous
	// process left it; this process recovers it.
	coretest.EnableLocalRefs(t, h.core, h.cfg)
	require.NoError(t, h.core.RefDurability.MarkDirty(h.ctx, ref, h.repoPath))
	markers, err := filepath.Glob(filepath.Join(h.cfg.Storages[0].Path, localrefs.MarkerDirName, "*.json"))
	require.NoError(t, err)
	require.Len(t, markers, 1)
	content, err := os.ReadFile(markers[0])
	require.NoError(t, err)
	content = []byte(strings.Replace(string(content), "}", `,"untrusted":"fsync: input/output error"}`, 1))
	require.NoError(t, os.WriteFile(markers[0], content, 0o600))

	coretest.EnableLocalRefs(t, h.core, h.cfg)
	_, err = h.core.RefDurability.Recover(h.ctx, []string{h.repo.GetStorageName()})
	require.NoError(t, err)
	require.True(t, h.core.RefDurability.Untrusted(h.repoPath))

	_, err = h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
	require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	_, err = h.core.Meta.GetRepository(h.ctx, corestore.KeyOf(h.repo))
	require.ErrorIs(t, err, corestore.ErrRepositoryNotFound, "nothing was imported")
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
