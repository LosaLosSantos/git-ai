package storagecore

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// A git write that the storage core did not record leaves objects that no pack
// holds. When a later changeset records a reference on top of them with a pack
// that leaves them out (built against the local references), git and the
// database agree on every reference and the local repository is connected,
// but the object storage can no longer rebuild the repository. The
// connectivity check of VerifyRepository reports it.
func TestVerifyRepositoryReportsObjectsMissingFromStorage(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeShadow, dialect)
		main := h.seed()
		repoID := h.importRepo()

		verify := h.verify(true)
		require.Empty(t, verify.GetDivergences())
		require.True(t, verify.GetConnectivityOk(), verify.GetConnectivityError())

		// Not recorded: main moves to b without the storage core.
		b := gittest.WriteCommit(t, h.cfg, h.repoPath, gittest.WithMessage("unrecorded"), gittest.WithParents(main))
		gittest.Exec(t, h.cfg, "-C", h.repoPath, "update-ref", "refs/heads/main", b.String())

		// Recorded on top of it with a pack that excludes what the local
		// references reach, so it does not hold b.
		c := gittest.WriteCommit(t, h.cfg, h.repoPath, gittest.WithMessage("recorded"), gittest.WithParents(b))
		pack, err := gitops.BuildPack(h.ctx, localrepo.NewTestRepo(t, h.cfg, h.repo), []git.ObjectID{c}, t.TempDir())
		require.NoError(t, err)
		require.False(t, pack.Empty())
		key := corestore.NewPackKey(repoID, pack.SHA256)
		f, err := pack.Open()
		require.NoError(t, err)
		require.NoError(t, h.core.Blobs.Put(h.ctx, key, f))
		require.NoError(t, f.Close())
		require.NoError(t, pack.Remove())
		_, err = h.core.Meta.CommitChangeset(h.ctx, corestore.ChangesetInput{
			RepoID:    repoID,
			Updates:   []corestore.RefUpdate{{Name: "refs/heads/main", Expect: corestore.ExpectAny, New: corestore.Target(c.String())}},
			Operation: "test",
			Actor:     "tester",
			NodeID:    "node-a",
			Force:     true,
			PackKey:   key,
			PackBytes: pack.Size,
		})
		require.NoError(t, err)
		gittest.Exec(t, h.cfg, "-C", h.repoPath, "update-ref", "refs/heads/main", c.String())

		// Without the connectivity check the repository looks consistent.
		verify = h.verify(false)
		require.Empty(t, verify.GetDivergences())

		verify = h.verify(true)
		require.Empty(t, verify.GetDivergences())
		require.False(t, verify.GetConnectivityOk())
		require.Contains(t, verify.GetConnectivityError(), "storage core objects: the packs of the database do not hold every object of its references")
		require.NotContains(t, verify.GetConnectivityError(), "\nstorage core objects", "the local check passed: only the storage message")
	})
}

// checkpointAtHead records a checkpoint of the repository at its database
// head, with a pack of everything its references reach built from the local
// repository, as checkpoint.Create does.
func (h *harness) checkpointAtHead(repoID int64) corestore.Checkpoint {
	h.t.Helper()

	refs, seq, err := h.core.Meta.Refs(h.ctx, repoID)
	require.NoError(h.t, err)
	var tips []git.ObjectID
	for _, target := range refs {
		if !target.IsSymbolic() {
			tips = append(tips, git.ObjectID(target))
		}
	}
	pack, err := gitops.BuildPackFrom(h.ctx, localrepo.NewTestRepo(h.t, h.cfg, h.repo), tips, h.t.TempDir())
	require.NoError(h.t, err)
	defer func() { require.NoError(h.t, pack.Remove()) }()
	key := corestore.NewPackKey(repoID, pack.SHA256)
	f, err := pack.Open()
	require.NoError(h.t, err)
	require.NoError(h.t, h.core.Blobs.Put(h.ctx, key, f))
	require.NoError(h.t, f.Close())

	cp, created, err := h.core.Meta.CreateCheckpoint(h.ctx, repoID, corestore.CheckpointInput{Seq: seq, Refs: refs, PackKey: key, PackBytes: pack.Size})
	require.NoError(h.t, err)
	require.True(h.t, created)
	return cp
}

// TestVerifyRepositoryAfterCompaction: the stored objects check rebuilds the
// repository the way a hydration does, from the newest checkpoint and the
// changesets after it, so it passes once garbage collection deleted the packs
// the checkpoint covers; a missing checkpoint pack is reported.
func TestVerifyRepositoryAfterCompaction(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeShadow, dialect)
		h.seed()
		repoID := h.importRepo()
		cp := h.checkpointAtHead(repoID)

		// Collected: every changeset pack up to the checkpoint.
		st, err := h.core.Meta.GCSnapshot(h.ctx, repoID)
		require.NoError(t, err)
		require.NotEmpty(t, st.Packs)
		for key, seq := range st.Packs {
			require.LessOrEqual(t, seq, cp.Seq)
			require.NoError(t, h.core.Blobs.Delete(h.ctx, key))
		}

		verify := h.verify(true)
		require.True(t, verify.GetConnectivityOk(), verify.GetConnectivityError())

		require.NoError(t, h.core.Blobs.Delete(h.ctx, cp.PackKey))
		verify = h.verify(true)
		require.False(t, verify.GetConnectivityOk())
		require.Contains(t, verify.GetConnectivityError(), "storage core objects: the pack \""+cp.PackKey+"\" of checkpoint")
	})
}

// slowReads is a bucket whose reads, once enabled, last until the end of their
// context. It is installed before the server starts and switched afterwards,
// so the core is never changed while the server reads it.
type slowReads struct {
	corestore.BlobStore
	enabled *atomic.Bool
}

func (b slowReads) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if b.enabled.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.BlobStore.Get(ctx, key)
}

// TestVerifyRepositoryStoredObjectsCheckIsBounded: the dry hydration runs under
// the write deadline W from its start (GC BOUNDARY rule, enforcement): one that
// outlives it is cancelled and fails as Unavailable.
func TestVerifyRepositoryStoredObjectsCheckIsBounded(t *testing.T) {
	var enabled atomic.Bool
	h := setup(t, corestore.ModeShadow, sqlstore.SQLite, func(core *corestore.Core) {
		// W = gc_grace - gc_margin = 3 s.
		core.GCGrace = 4 * time.Second
		core.GCMargin = time.Second
		core.Blobs = slowReads{BlobStore: core.Blobs, enabled: &enabled}
	})
	h.seed()
	h.importRepo()

	enabled.Store(true)
	_, err := h.client.VerifyRepository(h.ctx, &gitalypb.VerifyRepositoryRequest{Repository: h.repo, CheckConnectivity: true})
	require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "did not finish within the write deadline 3s")
}
