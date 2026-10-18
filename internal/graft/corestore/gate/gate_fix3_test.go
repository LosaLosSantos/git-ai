package gate_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gate"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/syncer"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// replacingFreshener imitates a catch-up that replaces the local copy by a
// hydrated one: the file changelog of the copy is gone afterwards.
type replacingFreshener struct {
	fakeFreshener
	path string
}

func (f *replacingFreshener) EnsureFresh(ctx context.Context, repo storage.Repository) error {
	if err := os.Remove(changelog.Path(f.path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return f.fakeFreshener.EnsureFresh(ctx, repo)
}

// The guard runs on the copy before the catch-up, which may replace it: a
// catch-up that removes the file changelog does not make the guard pass.
func TestGateHistoryGuardRunsBeforeCatchUp(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repo, path := untrackedRepo(t, ctx, cfg)
	fresh := &replacingFreshener{path: path}
	core := coretest.Open(t, coretest.Options{Mode: corestore.ModeAuthoritative})
	g := gate.New(core, fresh, testhelper.SharedLogger(t), config.NewLocator(cfg), localrepo.Factory{}, protoregistry.GitalyProtoPreregistered)
	registerHead(t, ctx, core, repo)

	_, err := changelog.Append(path, changelog.Meta{Operation: "shadow", Actor: "alice"}, []changelog.Update{
		{Reference: "refs/heads/main", OldOID: gittest.DefaultObjectHash.ZeroOID.String(), NewOID: gittest.DefaultObjectHash.EmptyTreeOID.String()},
	})
	require.NoError(t, err)

	handled := false
	_, err = g.UnaryInterceptor()(ctx, &gitalypb.HasLocalBranchesRequest{Repository: repo},
		&grpc.UnaryServerInfo{FullMethod: accessorMethod}, func(context.Context, any) (any, error) {
			handled = true
			return &gitalypb.HasLocalBranchesResponse{}, nil
		})
	require.False(t, handled)
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "error %v", err)
	require.Empty(t, fresh.called(), "no catch-up while the history is not imported")
	last, err := changelog.LastSequence(path)
	require.NoError(t, err)
	require.Equal(t, uint64(1), last)
}

// The shadow history guard with the real syncer:a files-backend repository
// imported in shadow mode, with a shadow-period file changelog entry the
// database does not have, is replaced by a reftable hydration on its first
// catch-up in authoritative mode (REFTABLE RULE). The guard must run on the
// copy as it is, before that catch-up: the request is refused, and the copy
// and its file changelog stay. After the history is imported, the request is
// served and the copy is replaced.
func TestGateHistoryGuardWithRealSyncerOnFilesBackendCopy(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	core := coretest.Open(t, coretest.Options{Mode: corestore.ModeAuthoritative})
	logger := testhelper.SharedLogger(t)
	locator := config.NewLocator(cfg)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)
	cache := catfile.NewCache(cfg)
	t.Cleanup(cache.Stop)
	repoFactory := localrepo.NewFactory(logger, locator, gitCmdFactory, cache)
	sy := syncer.New(core, logger, locator, repoFactory, gitCmdFactory)
	g := gate.New(core, sy, logger, locator, repoFactory, protoregistry.GitalyProtoPreregistered)

	repo, path := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
		ReferenceBackend:       git.ReferenceBackendFiles,
	})
	commit := gittest.WriteCommit(t, cfg, path, gittest.WithBranch("main"))
	local := repoFactory.Build(repo)

	// The import, as ImportRepository does it: register, upload the full pack,
	// commit changeset 1 with every reference, write the watermark.
	rec, _, err := core.Meta.RegisterRepository(ctx, corestore.KeyOf(repo), gittest.DefaultObjectHash.Format, corestore.InitialState{})
	require.NoError(t, err)
	pack, err := gitops.BuildFullPack(ctx, local, t.TempDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, pack.Remove()) }()
	packKey := corestore.NewPackKey(rec.ID, pack.SHA256)
	rc, err := pack.Open()
	require.NoError(t, err)
	require.NoError(t, core.Blobs.Put(ctx, packKey, rc))
	require.NoError(t, rc.Close())
	refs, err := gitops.ReadRefs(ctx, local)
	require.NoError(t, err)
	var updates []corestore.RefUpdate
	for name, target := range refs {
		updates = append(updates, corestore.RefUpdate{Name: name, Expect: corestore.ExpectValue, New: target})
	}
	_, err = core.Meta.CommitChangeset(ctx, corestore.ChangesetInput{
		RepoID: rec.ID, Updates: updates, Operation: "import", Actor: "import", NodeID: core.NodeID,
		PackKey: packKey, PackBytes: pack.Size,
	})
	require.NoError(t, err)
	require.NoError(t, gitops.WriteWatermark(path, corestore.Watermark{RepoID: rec.ID, Seq: 1}))

	// A write in the shadow period: only in the file changelog.
	shadow, err := changelog.Append(path, changelog.Meta{Operation: "shadow", Actor: "alice"}, []changelog.Update{
		{Reference: "refs/heads/main", OldOID: gittest.DefaultObjectHash.ZeroOID.String(), NewOID: commit.String()},
	})
	require.NoError(t, err)

	refFormat := func() string {
		return strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", path, "rev-parse", "--show-ref-format")))
	}
	call := func() (bool, error) {
		handled := false
		_, err := g.UnaryInterceptor()(ctx, &gitalypb.HasLocalBranchesRequest{Repository: repo},
			&grpc.UnaryServerInfo{FullMethod: accessorMethod}, func(context.Context, any) (any, error) {
				handled = true
				return &gitalypb.HasLocalBranchesResponse{}, nil
			})
		return handled, err
	}

	handled, err := call()
	require.False(t, handled)
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "error %v", err)
	require.Contains(t, err.Error(), "the shadow-mode history of this repository is not in the storage core yet: run graft storage import")
	last, err := changelog.LastSequence(path)
	require.NoError(t, err)
	require.Equal(t, uint64(1), last, "the file changelog is kept")
	require.Equal(t, "files", refFormat(), "the copy is not replaced")

	// The syncer refuses to replace the copy for callers outside the gate too.
	err = sy.EnsureFresh(ctx, repo)
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "error %v", err)
	require.Equal(t, "files", refFormat())

	_, _, err = core.Meta.ImportHistory(ctx, rec.ID, shadow, nil)
	require.NoError(t, err)

	handled, err = call()
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, "reftable", refFormat(), "the imported copy is replaced by a reftable one")
	dbRefs, _, err := core.Meta.Refs(ctx, rec.ID)
	require.NoError(t, err)
	localRefs, err := gitops.ReadRefs(ctx, repoFactory.Build(repo))
	require.NoError(t, err)
	require.Equal(t, dbRefs, localRefs)
}
