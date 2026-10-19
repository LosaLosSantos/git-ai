package coretest

import (
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// ImportOptions are the imported history of Import.
type ImportOptions struct {
	// Changelog and Reviews are recorded with the registration, as
	// ImportRepository records the file changelog and the file reviews.
	Changelog []changelog.Entry
	Reviews   []*gitalypb.Review
}

// Import imports the local repository into the core exactly as
// StorageCoreService.ImportRepository does (doc/graft/PLAN6.md, IMPORT RULE):
// it registers the repository with its history, uploads a pack of every object
// reachable from its references, commits changeset 1 with every reference
// (HEAD included) and writes the local watermark {id, 1}. Afterwards the
// repository is complete: EnsureFresh finds the local repository fresh, and
// any node can hydrate it. Use it instead of registering a repository by hand,
// whose head sequence 0 means an import that is not complete.
func Import(tb testing.TB, core *corestore.Core, repo *localrepo.Repo, repoProto *gitalypb.Repository, repoPath string, opts ImportOptions) corestore.Repository {
	tb.Helper()
	ctx := testhelper.Context(tb)

	hash, err := repo.ObjectHash(ctx)
	require.NoError(tb, err)

	refs, err := gitops.ReadRefs(ctx, repo)
	require.NoError(tb, err)

	registered, _, err := core.Meta.RegisterRepository(ctx, corestore.KeyOf(repoProto), hash.Format, corestore.InitialState{
		Changelog: opts.Changelog,
		Reviews:   opts.Reviews,
	})
	require.NoError(tb, err)

	pack, err := gitops.BuildFullPack(ctx, repo, tb.TempDir())
	require.NoError(tb, err)
	defer func() { _ = pack.Remove() }()

	// As ImportRepository: the write deadline starts at a read of the record
	// right before the upload (doc/graft/PLAN7.md, PACK OWNERSHIP RULE 4).
	var upload corestore.PackUpload
	if !pack.Empty() {
		readAt := time.Now()
		record, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repoProto))
		require.NoError(tb, err)
		require.Equal(tb, registered.ID, record.ID)

		f, err := pack.Open()
		require.NoError(tb, err)
		upload, err = core.UploadPack(ctx, record, readAt, pack.SHA256, pack.Size, f)
		require.NoError(tb, err)
		require.NoError(tb, f.Close())
	}

	updates := make([]corestore.RefUpdate, 0, len(refs))
	for name, target := range refs {
		updates = append(updates, corestore.RefUpdate{Name: name, Expect: corestore.ExpectValue, New: target})
	}

	commitCtx, cancel, err := upload.CommitContext(ctx)
	require.NoError(tb, err)
	defer cancel()

	res, err := core.Meta.CommitChangeset(commitCtx, corestore.ChangesetInput{
		RepoID:       registered.ID,
		Updates:      updates,
		Operation:    "import",
		Actor:        "import",
		NodeID:       core.NodeID,
		PackKey:      upload.Key,
		PackBytes:    upload.Bytes,
		PackDeadline: upload.DBDeadline,
	})
	require.NoError(tb, err)
	require.Equal(tb, uint64(1), res.Seq, "the import is changeset 1")

	require.NoError(tb, gitops.WriteWatermark(repoPath, corestore.Watermark{RepoID: registered.ID, Seq: res.Seq}))

	imported, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repoProto))
	require.NoError(tb, err)
	return imported
}
