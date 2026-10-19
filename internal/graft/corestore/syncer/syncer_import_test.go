package syncer_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestSyncerIncompleteImport checks the IMPORT RULE (doc/graft/PLAN6.md): a
// tracked repository with head sequence 0 is an import that is not complete.
// EnsureFresh returns FailedPrecondition and never hydrates, catches up, moves
// away or deletes the local repository, which is the only copy of what is being
// imported, whatever its reference backend and watermark. Once the import
// changeset is committed, the repository syncs normally.
func TestSyncerIncompleteImport(t *testing.T) {
	for _, backend := range []git.ReferenceBackend{git.ReferenceBackendFiles, git.ReferenceBackendReftables} {
		for _, tc := range []struct {
			desc      string
			watermark func(repoID int64) *corestore.Watermark
		}{
			{desc: "no watermark", watermark: func(int64) *corestore.Watermark { return nil }},
			{desc: "watermark of the registration", watermark: func(id int64) *corestore.Watermark { return &corestore.Watermark{RepoID: id} }},
			{desc: "watermark of another repository", watermark: func(id int64) *corestore.Watermark { return &corestore.Watermark{RepoID: id + 1000, Seq: 3} }},
		} {
			t.Run(backend.Name+"/"+tc.desc, func(t *testing.T) {
				forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
					testSyncerIncompleteImport(t, dialect, backend, tc.watermark)
				})
			})
		}
	}
}

func testSyncerIncompleteImport(t *testing.T, dialect sqlstore.Dialect, backend git.ReferenceBackend, watermark func(int64) *corestore.Watermark) {
	e := newEnv(t, envOptions{dialect: dialect})

	// The repository being imported: a branch, a tag and HEAD.
	_, path := gittest.CreateRepository(t, e.ctx, e.cfg, gittest.CreateRepositoryConfig{
		RelativePath:           localRelPath,
		SkipCreationViaService: true,
	})
	detected, err := gitcmd.DetectReferenceBackend(e.ctx, path)
	require.NoError(t, err)
	if detected.Name != backend.Name {
		gittest.Exec(t, e.cfg, "-C", path, "refs", "migrate", "--ref-format="+backend.Name)
		detected, err = gitcmd.DetectReferenceBackend(e.ctx, path)
		require.NoError(t, err)
	}
	require.Equal(t, backend.Name, detected.Name)
	main := gittest.WriteCommit(t, e.cfg, path, gittest.WithBranch("main"))
	gittest.WriteTag(t, e.cfg, path, "v1", main.Revision())

	local := e.localRepo()
	refs, err := gitops.ReadRefs(e.ctx, local)
	require.NoError(t, err)
	require.Len(t, refs, 3)

	// Step 1 of the import only: registered, no changeset.
	key := e.key(localRelPath)
	registered, _, err := e.core.Meta.RegisterRepository(e.ctx, key, gittest.DefaultObjectHash.Format, corestore.InitialState{})
	require.NoError(t, err)
	require.Zero(t, registered.HeadSeq)
	if w := watermark(registered.ID); w != nil {
		require.NoError(t, gitops.WriteWatermark(path, *w))
	}

	dirBefore, err := os.Stat(path)
	require.NoError(t, err)
	wmBefore, markedBefore, err := gitops.ReadWatermark(path)
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		err := e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath))
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "error: %v", err)
		require.ErrorContains(t, err, "repository import is not complete; run ImportRepository again")

		dirAfter, err := os.Stat(path)
		require.NoError(t, err)
		require.True(t, os.SameFile(dirBefore, dirAfter), "the local repository was moved away")
		after, err := gitops.ReadRefs(e.ctx, local)
		require.NoError(t, err)
		require.Equal(t, refs, after)
		wmAfter, markedAfter, err := gitops.ReadWatermark(path)
		require.NoError(t, err)
		require.Equal(t, markedBefore, markedAfter)
		require.Equal(t, wmBefore, wmAfter)
	}
	require.Equal(t, float64(2), syncCount(t, e.sy, "error"))
	require.Zero(t, syncCount(t, e.sy, "hydrated"))

	// The import completes: changeset 1 with every reference and the full pack,
	// then the watermark (ID, 1).
	pack, err := gitops.BuildFullPack(e.ctx, local, t.TempDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, pack.Remove()) }()
	packKey := corestore.NewPackKey(registered.ID, pack.SHA256)
	rc, err := pack.Open()
	require.NoError(t, err)
	require.NoError(t, e.core.Blobs.Put(e.ctx, packKey, rc))
	require.NoError(t, rc.Close())

	updates := make([]corestore.RefUpdate, 0, len(refs))
	for name, target := range refs {
		updates = append(updates, corestore.RefUpdate{Name: name, Expect: corestore.ExpectValue, New: target})
	}
	res, err := e.core.Meta.CommitChangeset(e.ctx, corestore.ChangesetInput{
		RepoID:    registered.ID,
		Updates:   updates,
		Operation: "import",
		Actor:     "tester",
		NodeID:    "node-a",
		PackKey:   packKey, PackDeadline: time.Now().Add(time.Hour),
		PackBytes: pack.Size,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, res.Seq)
	require.NoError(t, gitops.WriteWatermark(path, corestore.Watermark{RepoID: registered.ID, Seq: res.Seq}))

	// Now the repository syncs (a files-backend copy is replaced by a reftable
	// one hydrated from the import, REFTABLE RULE) and keeps every reference.
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	repo, err := e.core.Meta.GetRepository(e.ctx, key)
	require.NoError(t, err)
	e.verifySynced(t, repo)
	synced, err := gitops.ReadRefs(e.ctx, e.localRepo())
	require.NoError(t, err)
	require.Equal(t, refs, synced)
}
