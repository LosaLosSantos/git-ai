package storagecore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/coretest"
	"github.com/feder-cr/open_scion/internal/graft/corestore/gitops"
	"github.com/feder-cr/open_scion/internal/graft/corestore/sqlstore"
	reviewstore "github.com/feder-cr/open_scion/internal/graft/review/store"
	"github.com/feder-cr/open_scion/internal/grpc/client"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// harness runs a StorageCoreService over a repository that is not yet imported.
// The database of the storage core is reachable through core, so that tests
// can change it directly, as another node would.
type harness struct {
	t        *testing.T
	ctx      context.Context
	cfg      config.Cfg
	core     *corestore.Core
	client   gitalypb.StorageCoreServiceClient
	repo     *gitalypb.Repository
	repoPath string
}

// setup starts a server with the storage core in mode (ModeOff: no core) on
// the database dialect. Each adjust function may change the core before the
// server starts.
func setup(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect, adjust ...func(*corestore.Core)) *harness {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	// The repository is created on disk, not through the RepositoryService: in
	// authoritative mode that call would register the repository in the
	// database. The tests import it themselves.
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})

	var core *corestore.Core
	if mode != corestore.ModeOff {
		core = coretest.Open(t, coretest.Options{Mode: mode, Dialect: dialect})
		for _, fn := range adjust {
			fn(core)
		}
	}
	if mode == corestore.ModeAuthoritative {
		cfg.Graft.Standalone = true
	}

	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterStorageCoreServiceServer(srv, NewServer(deps))
	}, testserver.WithGraftStorage(core))

	conn, err := client.New(ctx, cfg.SocketPath)
	require.NoError(t, err)
	t.Cleanup(func() { testhelper.MustClose(t, conn) })

	return &harness{
		t:        t,
		ctx:      ctx,
		cfg:      cfg,
		core:     core,
		client:   gitalypb.NewStorageCoreServiceClient(conn),
		repo:     repo,
		repoPath: repoPath,
	}
}

// forEachCore runs fn for every mode with a storage core and every database
// dialect of the test environment.
func forEachCore(t *testing.T, fn func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect)) {
	for _, mode := range []corestore.Mode{corestore.ModeShadow, corestore.ModeAuthoritative} {
		for _, dialect := range coretest.Dialects() {
			t.Run(string(mode)+"/"+string(dialect), func(t *testing.T) {
				fn(t, mode, dialect)
			})
		}
	}
}

// forEachDialect runs fn for every database dialect of the test environment.
func forEachDialect(t *testing.T, fn func(t *testing.T, dialect sqlstore.Dialect)) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			fn(t, dialect)
		})
	}
}

// zeroOID returns the all-zero object ID of the test repositories.
func zeroOID() string {
	return gittest.DefaultObjectHash.ZeroOID.String()
}

// seed gives the repository a branch, a tag, two changelog entries and two
// reviews (one closed), and returns the commit of the branch.
func (h *harness) seed() git.ObjectID {
	h.t.Helper()

	main := gittest.WriteCommit(h.t, h.cfg, h.repoPath, gittest.WithBranch("main"))
	gittest.WriteTag(h.t, h.cfg, h.repoPath, "v1", main.Revision())

	_, err := changelog.Append(h.repoPath, changelog.Meta{Operation: "push", Actor: "alice"}, []changelog.Update{
		{Reference: "refs/heads/main", OldOID: zeroOID(), NewOID: main.String()},
	})
	require.NoError(h.t, err)
	_, err = changelog.Append(h.repoPath, changelog.Meta{Operation: "tag", Actor: "bob"}, []changelog.Update{
		{Reference: "refs/tags/v1", OldOID: zeroOID(), NewOID: main.String()},
	})
	require.NoError(h.t, err)

	store := reviewstore.Open(h.repoPath)
	_, err = store.Create(&gitalypb.Review{
		TargetBranch: []byte("main"),
		HeadCommitId: main.String(),
		Title:        []byte("fix x"),
	})
	require.NoError(h.t, err)
	_, err = store.Create(&gitalypb.Review{
		TargetBranch: []byte("main"),
		HeadCommitId: main.String(),
		Title:        []byte("closed one"),
		State:        gitalypb.Review_STATE_CLOSED,
	})
	require.NoError(h.t, err)

	return main
}

// importRepo imports the repository and returns the database ID it got.
func (h *harness) importRepo() int64 {
	h.t.Helper()

	_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
	require.NoError(h.t, err)

	state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
	require.NoError(h.t, err)
	require.True(h.t, state.GetTracked())
	return state.GetRepositoryId()
}

// verify runs VerifyRepository.
func (h *harness) verify(connectivity bool) *gitalypb.VerifyRepositoryResponse {
	h.t.Helper()

	resp, err := h.client.VerifyRepository(h.ctx, &gitalypb.VerifyRepositoryRequest{Repository: h.repo, CheckConnectivity: connectivity})
	require.NoError(h.t, err)
	return resp
}

// commitElsewhere writes a new commit in a repository of its own, uploads its
// pack and moves ref in the database to it, as another node would. The local
// repository does not get the commit until a catch-up.
func (h *harness) commitElsewhere(repoID int64, ref string) git.ObjectID {
	h.t.Helper()

	otherProto, otherPath := gittest.CreateRepository(h.t, h.ctx, h.cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	oid := gittest.WriteCommit(h.t, h.cfg, otherPath, gittest.WithMessage("written on another node"))

	pack, err := gitops.BuildPack(h.ctx, localrepo.NewTestRepo(h.t, h.cfg, otherProto), []git.ObjectID{oid}, h.t.TempDir())
	require.NoError(h.t, err)
	require.False(h.t, pack.Empty())
	key := corestore.PackKey(repoID, pack.SHA256)
	f, err := pack.Open()
	require.NoError(h.t, err)
	require.NoError(h.t, h.core.Blobs.Put(h.ctx, key, f))
	require.NoError(h.t, f.Close())

	_, err = h.core.Meta.CommitChangeset(h.ctx, corestore.ChangesetInput{
		RepoID: repoID,
		Updates: []corestore.RefUpdate{{
			Name:   ref,
			Expect: corestore.ExpectAny,
			New:    corestore.Target(oid.String()),
		}},
		Operation: "test",
		Actor:     "other-node",
		NodeID:    "node-b",
		PackKey:   key,
		PackBytes: pack.Size,
	})
	require.NoError(h.t, err)
	return oid
}

// localRefs reads the references of the local repository.
func (h *harness) localRefs() map[string]corestore.Target {
	h.t.Helper()

	refs, err := gitops.ReadRefs(h.ctx, localrepo.NewTestRepo(h.t, h.cfg, h.repo))
	require.NoError(h.t, err)
	return refs
}

func TestImportRepository(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect)
		main := h.seed()
		localBefore := h.localRefs()

		var fileLog []changelog.Entry
		require.NoError(t, changelog.Scan(h.repoPath, 0, func(e changelog.Entry) (bool, error) {
			fileLog = append(fileLog, e)
			return true, nil
		}))
		require.Len(t, fileLog, 2)

		stateBefore, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		require.False(t, stateBefore.GetTracked())

		res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.EqualValues(t, 1, res.GetSequence())
		require.EqualValues(t, 3, res.GetReferences(), "HEAD, refs/heads/main and refs/tags/v1")
		require.EqualValues(t, 2, res.GetChangelogEntries())
		require.EqualValues(t, 2, res.GetReviews())
		require.Positive(t, res.GetPackBytes())

		state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Equal(t, string(mode), state.GetMode())
		require.True(t, state.GetTracked())
		require.Positive(t, state.GetRepositoryId())
		require.EqualValues(t, 1, state.GetHeadSequence())
		require.Equal(t, "node-a", state.GetNodeId())
		repoID := state.GetRepositoryId()

		// The import writes the local watermark in both modes; only authoritative
		// mode reports it (shadow mode keeps no watermark of its own).
		w, ok, err := gitops.ReadWatermark(h.repoPath)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, corestore.Watermark{RepoID: repoID, Seq: 1}, w)
		if mode == corestore.ModeAuthoritative {
			require.EqualValues(t, 1, state.GetAppliedSequence())
		} else {
			require.EqualValues(t, 0, state.GetAppliedSequence())
		}

		// Every reference, HEAD included, is in the database.
		dbRefs, head, err := h.core.Meta.Refs(h.ctx, repoID)
		require.NoError(t, err)
		require.EqualValues(t, 1, head)
		require.Equal(t, localBefore, dbRefs)
		require.Equal(t, corestore.Target(main.String()), dbRefs["refs/heads/main"])
		require.True(t, dbRefs["HEAD"].IsSymbolic())

		// Changeset 1 is the import, with one uploaded pack.
		changesets, err := h.core.Meta.ListChangesets(h.ctx, repoID, 0, 1)
		require.NoError(t, err)
		require.Len(t, changesets, 1)
		require.Equal(t, "import", changesets[0].Operation)
		require.Equal(t, "node-a", changesets[0].NodeID)
		require.False(t, changesets[0].Forced)
		require.EqualValues(t, res.GetPackBytes(), changesets[0].PackBytes)
		exists, err := h.core.Blobs.Exists(h.ctx, changesets[0].PackKey)
		require.NoError(t, err)
		require.True(t, exists)

		// The changelog keeps its sequences; the import adds no entry.
		dbLog, err := h.core.Meta.ScanChangelog(h.ctx, repoID, 0, 0)
		require.NoError(t, err)
		require.Len(t, dbLog, len(fileLog))
		for i := range fileLog {
			require.Equal(t, fileLog[i].Sequence, dbLog[i].Sequence)
			require.Equal(t, fileLog[i].Reference, dbLog[i].Reference)
			require.Equal(t, fileLog[i].OldOID, dbLog[i].OldOID)
			require.Equal(t, fileLog[i].NewOID, dbLog[i].NewOID)
			require.Equal(t, fileLog[i].Operation, dbLog[i].Operation)
			require.Equal(t, fileLog[i].Actor, dbLog[i].Actor)
			require.True(t, fileLog[i].RecordedAt.Equal(dbLog[i].RecordedAt))
		}

		// The reviews keep their IDs, closed ones included.
		reviews, err := h.core.Meta.ListReviews(h.ctx, repoID, "", true, 0)
		require.NoError(t, err)
		require.Len(t, reviews, 2)
		require.Equal(t, "2", reviews[0].GetId())
		require.Equal(t, []byte("closed one"), reviews[0].GetTitle())
		require.Equal(t, "1", reviews[1].GetId())
		require.Equal(t, []byte("fix x"), reviews[1].GetTitle())

		// In authoritative mode the verification catches the local repository
		// up first (a files-backend repository is replaced by a reftable one
		// hydrated from the import pack); either way it is consistent.
		verify := h.verify(true)
		require.EqualValues(t, 1, verify.GetHeadSequence())
		require.Empty(t, verify.GetDivergences())
		require.True(t, verify.GetConnectivityOk(), verify.GetConnectivityError())
		require.Empty(t, verify.GetConnectivityError())
		require.Equal(t, localBefore, h.localRefs())
	})
}

func TestImportRepositoryEmpty(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect)

		res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.EqualValues(t, 1, res.GetSequence())
		require.EqualValues(t, 1, res.GetReferences(), "only HEAD")
		require.Zero(t, res.GetPackBytes())
		require.Zero(t, res.GetChangelogEntries())
		require.Zero(t, res.GetReviews())

		state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		changesets, err := h.core.Meta.ListChangesets(h.ctx, state.GetRepositoryId(), 0, 1)
		require.NoError(t, err)
		require.Len(t, changesets, 1)
		require.Empty(t, changesets[0].PackKey, "no objects, no pack")

		require.Empty(t, h.verify(false).GetDivergences())
	})
}

func TestImportRepositoryRefusals(t *testing.T) {
	t.Run("storage core off", func(t *testing.T) {
		h := setup(t, corestore.ModeOff, "")
		h.seed()

		_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
	})

	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect)
		h.seed()
		repoID := h.importRepo()

		// An import of a tracked repository only brings the history written
		// since the import (LOCK AND HISTORY RULE): there is none.
		res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.EqualValues(t, 1, res.GetSequence())
		require.Zero(t, res.GetReferences())
		require.Zero(t, res.GetPackBytes())
		require.Zero(t, res.GetChangelogEntries())
		require.Zero(t, res.GetReviews())

		// The second import changed nothing.
		w, ok, err := gitops.ReadWatermark(h.repoPath)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, corestore.Watermark{RepoID: repoID, Seq: 1}, w)

		_, err = h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{
			Repository: &gitalypb.Repository{StorageName: h.repo.GetStorageName(), RelativePath: "does/not/exist.git"},
		})
		require.Error(t, err)
		require.NotEqual(t, codes.OK, status.Code(err))

		_, err = h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{})
		testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
	})
}

func TestGetStorageState(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		h := setup(t, corestore.ModeOff, "")

		state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Equal(t, "off", state.GetMode())
		require.False(t, state.GetTracked())
		require.Empty(t, state.GetNodeId())
	})

	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect)
		h.seed()

		state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Equal(t, string(mode), state.GetMode())
		require.False(t, state.GetTracked())
		require.Zero(t, state.GetRepositoryId())
		require.Zero(t, state.GetHeadSequence())
		require.Zero(t, state.GetAppliedSequence())
		require.Equal(t, "node-a", state.GetNodeId())

		// A repository this node has no copy of is described too.
		state, err = h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{
			Repository: &gitalypb.Repository{StorageName: h.repo.GetStorageName(), RelativePath: "elsewhere.git"},
		})
		require.NoError(t, err)
		require.False(t, state.GetTracked())

		_, err = h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{})
		testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
	})
}

func TestVerifyRepositoryRefusals(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		h := setup(t, corestore.ModeOff, "")

		_, err := h.client.VerifyRepository(h.ctx, &gitalypb.VerifyRepositoryRequest{Repository: h.repo})
		testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
	})

	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect)
		h.seed()

		_, err := h.client.VerifyRepository(h.ctx, &gitalypb.VerifyRepositoryRequest{Repository: h.repo})
		testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
	})
}

func TestVerifyRepositoryShadowDivergence(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeShadow, dialect)
		main := h.seed()
		repoID := h.importRepo()

		// The database moves main (another node), and a branch appears locally
		// only (a write the database never saw).
		elsewhere := h.commitElsewhere(repoID, "refs/heads/main")
		rogue := gittest.WriteCommit(t, h.cfg, h.repoPath, gittest.WithBranch("a-rogue"))

		verify := h.verify(false)
		require.EqualValues(t, 2, verify.GetHeadSequence())
		require.Len(t, verify.GetDivergences(), 2)

		// Sorted by name.
		require.Equal(t, "refs/heads/a-rogue", string(verify.GetDivergences()[0].GetReference()))
		require.Equal(t, rogue.String(), verify.GetDivergences()[0].GetLocalTarget())
		require.Empty(t, verify.GetDivergences()[0].GetDatabaseTarget())

		require.Equal(t, "refs/heads/main", string(verify.GetDivergences()[1].GetReference()))
		require.Equal(t, main.String(), verify.GetDivergences()[1].GetLocalTarget())
		require.Equal(t, elsewhere.String(), verify.GetDivergences()[1].GetDatabaseTarget())

		// Shadow mode never changes the local repository.
		require.Equal(t, corestore.Target(main.String()), h.localRefs()["refs/heads/main"])
	})
}

func TestVerifyRepositoryAuthoritative(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeAuthoritative, dialect)
		h.seed()
		repoID := h.importRepo()

		// Verification catches the local repository up first, so a change made
		// on another node is not a divergence.
		elsewhere := h.commitElsewhere(repoID, "refs/heads/main")

		verify := h.verify(true)
		require.EqualValues(t, 2, verify.GetHeadSequence())
		require.Empty(t, verify.GetDivergences())
		require.True(t, verify.GetConnectivityOk(), verify.GetConnectivityError())
		require.Equal(t, corestore.Target(elsewhere.String()), h.localRefs()["refs/heads/main"])

		state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		require.EqualValues(t, 2, state.GetHeadSequence())
		require.EqualValues(t, 2, state.GetAppliedSequence())

		// A local change that bypassed the storage core (no hooks) is a
		// divergence: the catch-up does not touch references the database did
		// not change.
		gittest.Exec(t, h.cfg, "-C", h.repoPath, "update-ref", "refs/heads/rogue", elsewhere.String())

		verify = h.verify(false)
		require.Len(t, verify.GetDivergences(), 1)
		divergence := verify.GetDivergences()[0]
		require.Equal(t, "refs/heads/rogue", string(divergence.GetReference()))
		require.Equal(t, elsewhere.String(), divergence.GetLocalTarget())
		require.Empty(t, divergence.GetDatabaseTarget())
		require.False(t, verify.GetConnectivityOk(), "not requested")
	})
}

func TestVerifyRepositoryConnectivityFailure(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeShadow, dialect)
		main := h.seed()
		h.importRepo()

		// A commit whose tree is then deleted: the repository is no longer
		// connected.
		blob := gittest.WriteBlob(t, h.cfg, h.repoPath, []byte("lost content\n"))
		tree := gittest.WriteTree(t, h.cfg, h.repoPath, []gittest.TreeEntry{{Mode: "100644", Path: "lost.txt", OID: blob}})
		gittest.WriteCommit(t, h.cfg, h.repoPath, gittest.WithBranch("broken"), gittest.WithTree(tree), gittest.WithParents(main))
		hex := blob.String()
		require.NoError(t, os.Remove(filepath.Join(h.repoPath, "objects", hex[:2], hex[2:])))

		verify := h.verify(true)
		require.False(t, verify.GetConnectivityOk())
		require.NotEmpty(t, verify.GetConnectivityError())
		require.True(t, strings.Contains(verify.GetConnectivityError(), hex) ||
			strings.Contains(verify.GetConnectivityError(), "missing"), verify.GetConnectivityError())
	})
}
