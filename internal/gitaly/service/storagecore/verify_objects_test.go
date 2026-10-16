package storagecore

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
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
		key := corestore.PackKey(repoID, pack.SHA256)
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
