package syncer_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/syncer"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// localRelPath is the relative path of the repository synced by the tests.
const localRelPath = "synced.git"

// forEachDialect runs the test once per database dialect (SQLite, and
// Postgres when GRAFT_TEST_POSTGRES_DSN is set).
func forEachDialect(t *testing.T, test func(t *testing.T, dialect sqlstore.Dialect)) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			test(t, dialect)
		})
	}
}

// env is one node: a storage with a local repository cache, a storage core and
// a syncer, plus helpers to build changesets from a source repository.
type env struct {
	ctx           context.Context
	cfg           config.Cfg
	storageName   string
	storagePath   string
	logger        log.Logger
	locator       storage.Locator
	gitCmdFactory gitcmd.CommandFactory
	repoFactory   localrepo.Factory
	core          *corestore.Core
	sy            *syncer.Syncer
	// databaseURL and bucketURL are those of the core, for a second node.
	databaseURL string
	bucketURL   string
}

// bucketDir is the directory of the file bucket.
func (e *env) bucketDir() string {
	return strings.TrimPrefix(e.bucketURL, "file://")
}

type envOptions struct {
	dialect     sqlstore.Dialect
	databaseURL string
	bucketURL   string
	mode        corestore.Mode
}

func newEnv(t *testing.T, opts envOptions) *env {
	t.Helper()

	cfg := testcfg.Build(t)
	ctx := testhelper.Context(t)
	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(cfg)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)
	cache := catfile.NewCache(cfg)
	t.Cleanup(cache.Stop)
	repoFactory := localrepo.NewFactory(logger, locator, gitCmdFactory, cache)

	if opts.dialect == "" {
		opts.dialect = sqlstore.SQLite
	}
	if opts.databaseURL == "" {
		opts.databaseURL = coretest.DatabaseURL(t, opts.dialect)
	}
	if opts.bucketURL == "" {
		opts.bucketURL = coretest.BucketURL(t, false)
	}

	core := coretest.Open(t, coretest.Options{
		Mode:        opts.mode,
		Dialect:     opts.dialect,
		DatabaseURL: opts.databaseURL,
		BucketURL:   opts.bucketURL,
	})

	return &env{
		ctx:           ctx,
		cfg:           cfg,
		storageName:   cfg.Storages[0].Name,
		storagePath:   cfg.Storages[0].Path,
		logger:        logger,
		locator:       locator,
		gitCmdFactory: gitCmdFactory,
		repoFactory:   repoFactory,
		core:          core,
		sy:            syncer.New(core, logger, locator, repoFactory, gitCmdFactory),
		databaseURL:   opts.databaseURL,
		bucketURL:     opts.bucketURL,
	}
}

// secondNode returns another node (own storage, own syncer) sharing the
// database and the bucket of e.
func (e *env) secondNode(t *testing.T, dialect sqlstore.Dialect) *env {
	t.Helper()
	return newEnv(t, envOptions{dialect: dialect, databaseURL: e.databaseURL, bucketURL: e.bucketURL})
}

func (e *env) pbRepo(relativePath string) *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: e.storageName, RelativePath: relativePath}
}

func (e *env) key(relativePath string) corestore.RepoKey {
	return corestore.RepoKey{Storage: e.storageName, RelativePath: relativePath}
}

func (e *env) localPath() string {
	return filepath.Join(e.storagePath, localRelPath)
}

// localRepo returns the local cache repository of the synced path.
func (e *env) localRepo() *localrepo.Repo {
	return e.repoFactory.Build(e.pbRepo(localRelPath))
}

// source is the repository the test commits are written to. Its own references
// stay empty, so that the packs of the test changesets contain whole histories.
type source struct {
	repo *localrepo.Repo
	path string
}

func (e *env) newSource(t *testing.T) source {
	t.Helper()

	pb, path := gittest.CreateRepository(t, e.ctx, e.cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	return source{repo: localrepo.NewTestRepo(t, e.cfg, pb), path: path}
}

func (e *env) writeCommit(t *testing.T, src source, parents ...git.ObjectID) git.ObjectID {
	t.Helper()
	return gittest.WriteCommit(t, e.cfg, src.path, gittest.WithParents(parents...))
}

// changeset uploads the pack of the tips (if any) and commits the updates as
// the next changeset of repository id. It returns the result and the pack key.
func (e *env) changeset(t *testing.T, id int64, src source, updates []corestore.RefUpdate, tips ...git.ObjectID) (corestore.CommitResult, string) {
	t.Helper()

	pack, err := gitops.BuildPack(e.ctx, src.repo, tips, t.TempDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, pack.Remove()) }()

	var key string
	in := corestore.ChangesetInput{
		RepoID:    id,
		Updates:   updates,
		Operation: "test",
		Actor:     "tester",
		NodeID:    "node-a",
	}
	if !pack.Empty() {
		key = corestore.PackKey(id, pack.SHA256)
		rc, err := pack.Open()
		require.NoError(t, err)
		require.NoError(t, e.core.Blobs.Put(e.ctx, key, rc))
		require.NoError(t, rc.Close())
		in.PackKey = key
		in.PackBytes = pack.Size
	}

	res, err := e.core.Meta.CommitChangeset(e.ctx, in)
	require.NoError(t, err)
	require.False(t, res.NoOp)
	return res, key
}

// register creates the repository of key in the database with a first
// changeset that points main (and HEAD) at head.
func (e *env) register(t *testing.T, key corestore.RepoKey, src source, head git.ObjectID) corestore.Repository {
	t.Helper()

	repo, _, err := e.core.Meta.RegisterRepository(e.ctx, key, gittest.DefaultObjectHash.Format, corestore.InitialState{})
	require.NoError(t, err)

	e.changeset(t, repo.ID, src, []corestore.RefUpdate{
		{Name: "HEAD", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")},
		{Name: "refs/heads/main", Expect: corestore.ExpectValue, New: corestore.Target(head)},
	}, head)

	repo, err = e.core.Meta.GetRepository(e.ctx, key)
	require.NoError(t, err)
	return repo
}

// advanceMain commits a changeset moving main from old to a new child commit
// and returns the new commit.
func (e *env) advanceMain(t *testing.T, id int64, src source, old git.ObjectID) git.ObjectID {
	t.Helper()

	next := e.writeCommit(t, src, old)
	e.changeset(t, id, src, []corestore.RefUpdate{
		{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(old), New: corestore.Target(next)},
	}, next)
	return next
}

// verifySynced checks that the local repository of localRelPath has exactly
// the database references, the watermark of the repository and no
// connectivity errors.
func (e *env) verifySynced(t *testing.T, repo corestore.Repository) {
	t.Helper()

	dbRefs, head, err := e.core.Meta.Refs(e.ctx, repo.ID)
	require.NoError(t, err)

	localRefs, err := gitops.ReadRefs(e.ctx, e.localRepo())
	require.NoError(t, err)
	require.Equal(t, dbRefs, localRefs)

	wm, ok, err := gitops.ReadWatermark(e.localPath())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, corestore.Watermark{RepoID: repo.ID, Seq: head}, wm)

	gittest.Exec(t, e.cfg, "-C", e.localPath(), "fsck", "--connectivity-only")
}

// counter returns the value of the counter family name (with the result label
// when result is not empty).
func counter(t *testing.T, sy *syncer.Syncer, name, result string) float64 {
	t.Helper()

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(sy))
	families, err := reg.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if result == "" {
				return metric.GetCounter().GetValue()
			}
			for _, label := range metric.GetLabel() {
				if label.GetName() == "result" && label.GetValue() == result {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func syncCount(t *testing.T, sy *syncer.Syncer, result string) float64 {
	t.Helper()
	return counter(t, sy, "graft_storage_sync_total", result)
}

func TestSyncerHydratesEmptyStorage(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		c2 := e.writeCommit(t, src, c1)
		repo := e.register(t, e.key(localRelPath), src, c1)
		e.changeset(t, repo.ID, src, []corestore.RefUpdate{
			{Name: "refs/tags/v1", Expect: corestore.ExpectValue, New: corestore.Target(c2)},
		}, c2)
		repo, err := e.core.Meta.GetRepository(e.ctx, repo.Key)
		require.NoError(t, err)

		_, ok, err := e.sy.Applied(e.ctx, e.pbRepo(localRelPath))
		require.NoError(t, err)
		require.False(t, ok, "no watermark before the first sync")

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		require.Equal(t, float64(1), syncCount(t, e.sy, "hydrated"))
		require.Equal(t, float64(2), counter(t, e.sy, "graft_storage_sync_changesets_total", ""))
		require.Positive(t, counter(t, e.sy, "graft_storage_sync_bytes_total", ""))

		// The hydrated repository has the recorded object format and HEAD.
		refs, err := gitops.ReadRefs(e.ctx, e.localRepo())
		require.NoError(t, err)
		require.Equal(t, corestore.SymbolicTarget("refs/heads/main"), refs["HEAD"])
		hash, err := e.localRepo().ObjectHash(e.ctx)
		require.NoError(t, err)
		require.Equal(t, gittest.DefaultObjectHash.Format, hash.Format)

		w, ok, err := e.sy.Applied(e.ctx, e.pbRepo(localRelPath))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, corestore.Watermark{RepoID: repo.ID, Seq: 2}, w)

		// The next call finds the repository fresh.
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		require.Equal(t, float64(1), syncCount(t, e.sy, "fresh"))
	})
}

func TestSyncerHydratesNestedPath(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		const nested = "group/sub/project.git"
		repo := e.register(t, e.key(nested), src, c1)

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(nested)))

		local := e.repoFactory.Build(e.pbRepo(nested))
		refs, err := gitops.ReadRefs(e.ctx, local)
		require.NoError(t, err)
		dbRefs, _, err := e.core.Meta.Refs(e.ctx, repo.ID)
		require.NoError(t, err)
		require.Equal(t, dbRefs, refs)
	})
}

func TestSyncerCatchesUp(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		c2 := e.writeCommit(t, src, c1)
		e.changeset(t, repo.ID, src, []corestore.RefUpdate{
			{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: corestore.Target(c2)},
			{Name: "refs/heads/feature", Expect: corestore.ExpectValue, New: corestore.Target(c2)},
		}, c2)

		// A changeset without objects (no pack) that deletes the feature branch.
		e.changeset(t, repo.ID, src, []corestore.RefUpdate{
			{Name: "refs/heads/feature", Expect: corestore.ExpectValue, Old: corestore.Target(c2)},
		})

		// HEAD moves to another branch.
		e.changeset(t, repo.ID, src, []corestore.RefUpdate{
			{Name: "refs/heads/other", Expect: corestore.ExpectValue, New: corestore.Target(c1)},
			{Name: "HEAD", Expect: corestore.ExpectAny, New: corestore.SymbolicTarget("refs/heads/other")},
		})

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		require.Equal(t, float64(1), syncCount(t, e.sy, "hydrated"))
		require.Equal(t, float64(1), syncCount(t, e.sy, "caught_up"))

		refs, err := gitops.ReadRefs(e.ctx, e.localRepo())
		require.NoError(t, err)
		require.NotContains(t, refs, "refs/heads/feature")
		require.Equal(t, corestore.Target(c2), refs["refs/heads/main"])
		require.Equal(t, corestore.SymbolicTarget("refs/heads/other"), refs["HEAD"])

		// A second call is a no-op.
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		require.Equal(t, float64(1), syncCount(t, e.sy, "fresh"))
	})
}

func TestSyncerSecondNodeCatchesUp(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		a := newEnv(t, envOptions{dialect: dialect})
		b := a.secondNode(t, dialect)

		src := a.newSource(t)
		c1 := a.writeCommit(t, src)
		repo := a.register(t, a.key(localRelPath), src, c1)

		require.NoError(t, a.sy.EnsureFresh(a.ctx, a.pbRepo(localRelPath)))
		require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))

		c2 := a.advanceMain(t, repo.ID, src, c1)
		require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))
		b.verifySynced(t, repo)

		refs, err := gitops.ReadRefs(b.ctx, b.localRepo())
		require.NoError(t, err)
		require.Equal(t, corestore.Target(c2), refs["refs/heads/main"])
	})
}

func TestSyncerRehydratesOnRepoIDMismatch(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		// A ref only the stale local repository knows about, and a watermark of
		// another repository ID.
		require.NoError(t, gitops.ApplyRefs(e.ctx, e.localRepo(), map[string]corestore.Target{
			"refs/heads/local-only": corestore.Target(c1),
		}))
		require.NoError(t, gitops.WriteWatermark(e.localPath(), corestore.Watermark{RepoID: repo.ID + 1000, Seq: 1}))

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		require.Equal(t, float64(2), syncCount(t, e.sy, "hydrated"))

		refs, err := gitops.ReadRefs(e.ctx, e.localRepo())
		require.NoError(t, err)
		require.NotContains(t, refs, "refs/heads/local-only")
	})
}

func TestSyncerRehydratesRecreatedRepository(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		old := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		// The repository is removed and created again (on another node): it gets
		// a new ID, so the local cache of the old one is stale.
		deleted, err := e.core.Meta.DeleteRepository(e.ctx, e.key(localRelPath))
		require.NoError(t, err)
		require.True(t, deleted)

		c2 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c2)
		require.NotEqual(t, old.ID, repo.ID)

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		require.Equal(t, float64(2), syncCount(t, e.sy, "hydrated"))
	})
}

// TestSyncerLateNoteOfReplacedRepositoryIgnored covers a write that was still
// in flight in a local repository replaced by a hydration (the database
// repository was deleted and created again): its NoteCommitted arrives after
// the install, with the ID of the old repository, and must not move the
// watermark of the hydrated one, which lacks that write.
func TestSyncerLateNoteOfReplacedRepositoryIgnored(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		old := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		deleted, err := e.core.Meta.DeleteRepository(e.ctx, e.key(localRelPath))
		require.NoError(t, err)
		require.True(t, deleted)
		c2 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c2)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)

		// The late notes of the old repository: the next sequences after the
		// hydrated watermark.
		e.sy.NoteCommitted(e.ctx, e.localPath(), old.ID, 2)
		e.sy.NoteCommitted(e.ctx, e.localPath(), old.ID, 3)

		wm, ok, err := gitops.ReadWatermark(e.localPath())
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, corestore.Watermark{RepoID: repo.ID, Seq: 1}, wm)

		// The next changeset of the new repository is fetched, not assumed.
		c3 := e.advanceMain(t, repo.ID, src, c2)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		refs, err := gitops.ReadRefs(e.ctx, e.localRepo())
		require.NoError(t, err)
		require.Equal(t, corestore.Target(c3), refs["refs/heads/main"])
	})
}

func TestSyncerHydratesOverInvalidOrUnmarkedLocalRepository(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)

		// A directory that is not a git repository is in the way.
		require.NoError(t, os.MkdirAll(filepath.Join(e.localPath(), "junk"), 0o750))
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)

		// A corrupt watermark is treated as no watermark.
		require.NoError(t, os.WriteFile(gitops.WatermarkPath(e.localPath()), []byte("{not json"), 0o600))
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
		require.Equal(t, float64(2), syncCount(t, e.sy, "hydrated"))
	})
}

func TestSyncerUntrackedAndNotFound(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})

		// A local repository that the database does not know.
		gittest.CreateRepository(t, e.ctx, e.cfg, gittest.CreateRepositoryConfig{
			RelativePath:           "untracked.git",
			SkipCreationViaService: true,
		})

		err := e.sy.EnsureFresh(e.ctx, e.pbRepo("untracked.git"))
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "error: %v", err)
		require.Contains(t, err.Error(), "repository is not imported into the storage core")

		err = e.sy.EnsureFresh(e.ctx, e.pbRepo("missing.git"))
		require.Equal(t, codes.NotFound, status.Code(err), "error: %v", err)
		require.Equal(t, float64(2), syncCount(t, e.sy, "error"))
	})
}

func TestSyncerDatabaseUnavailable(t *testing.T) {
	e := newEnv(t, envOptions{})
	require.NoError(t, e.core.Meta.Close())

	err := e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath))
	require.Equal(t, codes.Unavailable, status.Code(err), "error: %v", err)
}

func TestSyncerNoopUnlessAuthoritative(t *testing.T) {
	for _, mode := range []corestore.Mode{corestore.ModeShadow, corestore.ModeOff} {
		t.Run(string(mode), func(t *testing.T) {
			e := newEnv(t, envOptions{mode: mode})

			// Not even an untracked repository is an error.
			require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo("missing.git")))
			_, ok, err := e.sy.Applied(e.ctx, e.pbRepo("missing.git"))
			require.NoError(t, err)
			require.False(t, ok)
		})
	}

	var nilSyncer *syncer.Syncer
	ctx := testhelper.Context(t)
	require.NoError(t, nilSyncer.EnsureFresh(ctx, &gitalypb.Repository{StorageName: "default", RelativePath: "x.git"}))
	nilSyncer.NoteCommitted(ctx, "/nowhere", 1, 1)
}

func TestSyncerConcurrentEnsureFreshSyncsOnce(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)

		const callers = 16
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, callers)
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath))
			}()
		}
		close(start)
		wg.Wait()
		close(errs)

		for err := range errs {
			require.NoError(t, err)
		}
		e.verifySynced(t, repo)
		require.Equal(t, float64(1), syncCount(t, e.sy, "hydrated"), "the repository is hydrated exactly once")
		require.Zero(t, syncCount(t, e.sy, "error"))
	})
}

func TestSyncerNoteCommittedContiguity(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		prev := c1
		for range 5 {
			prev = e.advanceMain(t, repo.ID, src, prev)
		}

		watermark := func() uint64 {
			t.Helper()
			w, ok, err := e.sy.Applied(e.ctx, e.pbRepo(localRelPath))
			require.NoError(t, err)
			require.True(t, ok)
			return w.Seq
		}

		// Sequence 3 out of order: sequence 2 is missing, nothing advances.
		e.sy.NoteCommitted(e.ctx, e.localPath(), repo.ID, 3)
		require.Equal(t, uint64(1), watermark())

		// Sequence 2 closes the gap: the watermark moves through 2 and 3.
		e.sy.NoteCommitted(e.ctx, e.localPath(), repo.ID, 2)
		require.Equal(t, uint64(3), watermark())

		// A sequence of another repository ID is ignored.
		e.sy.NoteCommitted(e.ctx, e.localPath(), repo.ID+1000, 4)
		require.Equal(t, uint64(3), watermark())

		// Sequence 5 with 4 missing: nothing advances; 4 closes the gap.
		e.sy.NoteCommitted(e.ctx, e.localPath(), repo.ID, 5)
		require.Equal(t, uint64(3), watermark())
		e.sy.NoteCommitted(e.ctx, e.localPath(), repo.ID, 4)
		require.Equal(t, uint64(5), watermark())

		// Sequence 6 is left as a gap: the next EnsureFresh downloads it.
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		require.Equal(t, uint64(6), watermark())
		require.Equal(t, float64(1), syncCount(t, e.sy, "caught_up"))
	})
}

func TestSyncerEnsureFreshUsesNotedSequences(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		// The node made changeset 2 itself: its pack is not needed. Removing it
		// from the bucket proves that EnsureFresh does not download it.
		c2 := e.writeCommit(t, src, c1)
		_, key := e.changeset(t, repo.ID, src, []corestore.RefUpdate{
			{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: corestore.Target(c2)},
		}, c2)
		require.NoError(t, os.Remove(filepath.Join(e.bucketDir(), key)))
		// The local write of the node (its objects and its reference).
		require.NoError(t, gitops.IndexPack(e.ctx, e.localRepo(), packOf(t, e, src, c2)))
		require.NoError(t, gitops.ApplyRefs(e.ctx, e.localRepo(), map[string]corestore.Target{
			"refs/heads/main": corestore.Target(c2),
		}))

		e.sy.NoteCommitted(e.ctx, e.localPath(), repo.ID, 2)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		require.Equal(t, float64(1), syncCount(t, e.sy, "fresh"))
		require.Zero(t, syncCount(t, e.sy, "caught_up"))
		e.verifySynced(t, repo)
	})
}

// packOf returns a pack with the history of tip from the source repository.
func packOf(t *testing.T, e *env, src source, tip git.ObjectID) *bytes.Reader {
	t.Helper()

	pack, err := gitops.BuildPack(e.ctx, src.repo, []git.ObjectID{tip}, t.TempDir())
	require.NoError(t, err)
	require.False(t, pack.Empty())
	defer func() { require.NoError(t, pack.Remove()) }()

	data, err := os.ReadFile(pack.Path)
	require.NoError(t, err)
	return bytes.NewReader(data)
}

func TestSyncerRepairsDivergedLocalReference(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		// The local main points to a commit no changeset ever recorded.
		rogue := gittest.WriteCommit(t, e.cfg, e.localPath(), gittest.WithMessage("rogue"))
		require.NoError(t, gitops.ApplyRefs(e.ctx, e.localRepo(), map[string]corestore.Target{
			"refs/heads/main": corestore.Target(rogue),
		}))

		e.advanceMain(t, repo.ID, src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		e.verifySynced(t, repo)
	})
}

// A catch-up that finds, when it reads the database head again, that the
// repository was removed and registered again under a new ID restarts the sync
// with the new record (re-read), so it hydrates the new repository instead of
// looping on the old one until the sync budget runs out.
func TestSyncerRestartRereadsRecreatedRepository(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		old := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		// A local value no changeset explains makes the catch-up read the head
		// again; the old repository has one more changeset to catch up.
		rogue := gittest.WriteCommit(t, e.cfg, e.localPath(), gittest.WithMessage("rogue"))
		require.NoError(t, gitops.ApplyRefs(e.ctx, e.localRepo(), map[string]corestore.Target{
			"refs/heads/main": corestore.Target(rogue),
		}))
		e.advanceMain(t, old.ID, src, c1)

		// After the range of the old repository is fetched, the repository is
		// removed and created again (on another node).
		var repo corestore.Repository
		e.sy.SetTestHook(once("fetched", func() {
			deleted, err := e.core.Meta.DeleteRepository(e.ctx, e.key(localRelPath))
			require.NoError(t, err)
			require.True(t, deleted)
			repo = e.register(t, e.key(localRelPath), src, e.writeCommit(t, src))
		}))

		ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
		defer cancel()
		require.NoError(t, e.sy.EnsureFresh(ctx, e.pbRepo(localRelPath)))
		require.NotEqual(t, old.ID, repo.ID)
		e.verifySynced(t, repo)
		require.Equal(t, float64(2), syncCount(t, e.sy, "hydrated"))
		require.Zero(t, syncCount(t, e.sy, "error"))
	})
}

func TestSyncerLockContentionRetries(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		e.advanceMain(t, repo.ID, src, c1)

		// A reference transaction of a concurrent writer holds the lock of main
		// until it is closed.
		u := lockMain(t, e, c1)

		released := make(chan struct{})
		go func() {
			defer close(released)
			time.Sleep(300 * time.Millisecond)
			_ = u.Close()
		}()

		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		<-released
		e.verifySynced(t, repo)
		require.Equal(t, float64(1), syncCount(t, e.sy, "caught_up"))
	})
}

func TestSyncerLockContentionTimesOut(t *testing.T) {
	e := newEnv(t, envOptions{})
	src := e.newSource(t)
	c1 := e.writeCommit(t, src)
	repo := e.register(t, e.key(localRelPath), src, c1)
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

	e.advanceMain(t, repo.ID, src, c1)

	// A short budget for the contended sync only.
	e.core.SyncTimeout = 500 * time.Millisecond

	u := lockMain(t, e, c1)
	err := e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath))
	require.Equal(t, codes.Unavailable, status.Code(err), "error: %v", err)

	// The watermark did not move.
	wm, ok, err := gitops.ReadWatermark(e.localPath())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, uint64(1), wm.Seq)

	// Once the lock is gone the sync succeeds (a call may still join the end
	// of the timed-out sync, hence the retries).
	require.NoError(t, u.Close())
	e.core.SyncTimeout = 10 * time.Second
	require.Eventually(t, func() bool {
		return e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)) == nil
	}, 30*time.Second, 50*time.Millisecond)
	e.verifySynced(t, repo)
}

// lockMain starts and prepares a reference transaction on the local main, so
// that git holds its lock until the returned updater is closed.
func lockMain(t *testing.T, e *env, value git.ObjectID) *updateref.Updater {
	t.Helper()

	u, err := updateref.New(e.ctx, e.localRepo(), updateref.WithDisabledTransactions())
	require.NoError(t, err)
	require.NoError(t, u.Start())
	require.NoError(t, u.Update(git.ReferenceName("refs/heads/main"), value, ""))
	require.NoError(t, u.Prepare())
	return u
}

func TestSyncerCorruptPackFailsWithoutTouchingReferences(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		bucketURL := coretest.BucketURL(t, false)
		e := newEnv(t, envOptions{dialect: dialect, bucketURL: bucketURL})
		src := e.newSource(t)
		c1 := e.writeCommit(t, src)
		repo := e.register(t, e.key(localRelPath), src, c1)
		require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		c2 := e.writeCommit(t, src, c1)
		_, key := e.changeset(t, repo.ID, src, []corestore.RefUpdate{
			{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: corestore.Target(c2)},
		}, c2)
		require.NotEmpty(t, key)

		// Replace the pack in the file bucket with other content.
		bucketDir := strings.TrimPrefix(bucketURL, "file://")
		require.NoError(t, os.WriteFile(filepath.Join(bucketDir, key), []byte("not the pack"), 0o600))

		require.Error(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

		refs, err := gitops.ReadRefs(e.ctx, e.localRepo())
		require.NoError(t, err)
		require.Equal(t, corestore.Target(c1), refs["refs/heads/main"])

		wm, ok, err := gitops.ReadWatermark(e.localPath())
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, corestore.Watermark{RepoID: repo.ID, Seq: 1}, wm)
		require.Equal(t, float64(1), syncCount(t, e.sy, "error"))

		// A missing pack fails the same way.
		require.NoError(t, os.Remove(filepath.Join(bucketDir, key)))
		require.Error(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
		refs, err = gitops.ReadRefs(e.ctx, e.localRepo())
		require.NoError(t, err)
		require.Equal(t, corestore.Target(c1), refs["refs/heads/main"])
	})
}

func TestSyncerInvalidDatabaseContentIsRejected(t *testing.T) {
	for _, tc := range []struct {
		desc   string
		update func(c1 git.ObjectID) corestore.RefUpdate
	}{
		{
			desc: "reference name with two dots",
			update: func(c1 git.ObjectID) corestore.RefUpdate {
				return corestore.RefUpdate{Name: "refs/heads/bad..name", Expect: corestore.ExpectValue, New: corestore.Target(c1)}
			},
		},
		{
			desc: "reference name escaping refs",
			update: func(c1 git.ObjectID) corestore.RefUpdate {
				return corestore.RefUpdate{Name: "refs/../config", Expect: corestore.ExpectValue, New: corestore.Target(c1)}
			},
		},
		{
			desc: "reference name with a space",
			update: func(c1 git.ObjectID) corestore.RefUpdate {
				return corestore.RefUpdate{Name: "refs/heads/a b", Expect: corestore.ExpectValue, New: corestore.Target(c1)}
			},
		},
		{
			desc: "target that is not an object ID",
			update: func(git.ObjectID) corestore.RefUpdate {
				return corestore.RefUpdate{Name: "refs/heads/x", Expect: corestore.ExpectValue, New: "--upload-pack=evil"}
			},
		},
		{
			desc: "symbolic target with an invalid name",
			update: func(git.ObjectID) corestore.RefUpdate {
				return corestore.RefUpdate{Name: "refs/heads/sym", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/x:y")}
			},
		},
		{
			// gitops.ValidateTarget accepts it (ValidateName allows HEAD), git
			// does not: it must be rejected before the plain moves are applied.
			desc: "HEAD pointing to itself",
			update: func(git.ObjectID) corestore.RefUpdate {
				return corestore.RefUpdate{Name: "HEAD", Expect: corestore.ExpectValue, Old: corestore.SymbolicTarget("refs/heads/main"), New: corestore.SymbolicTarget("HEAD")}
			},
		},
		{
			desc: "symbolic target HEAD",
			update: func(git.ObjectID) corestore.RefUpdate {
				return corestore.RefUpdate{Name: "refs/heads/sym", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("HEAD")}
			},
		},
		{
			desc: "symbolic reference pointing to itself",
			update: func(git.ObjectID) corestore.RefUpdate {
				return corestore.RefUpdate{Name: "refs/heads/sym", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/sym")}
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			e := newEnv(t, envOptions{})
			src := e.newSource(t)
			c1 := e.writeCommit(t, src)
			repo := e.register(t, e.key(localRelPath), src, c1)
			require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

			// A valid update in the same range must not be applied either.
			c2 := e.writeCommit(t, src, c1)
			e.changeset(t, repo.ID, src, []corestore.RefUpdate{
				{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: corestore.Target(c2)},
			}, c2)
			e.changeset(t, repo.ID, src, []corestore.RefUpdate{tc.update(c1)})

			err := e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath))
			require.Error(t, err)
			require.Equal(t, codes.Internal, status.Code(err), "error: %v", err)

			refs, err := gitops.ReadRefs(e.ctx, e.localRepo())
			require.NoError(t, err)
			require.Equal(t, map[string]corestore.Target{
				"HEAD":            corestore.SymbolicTarget("refs/heads/main"),
				"refs/heads/main": corestore.Target(c1),
			}, refs)

			wm, ok, err := gitops.ReadWatermark(e.localPath())
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, uint64(1), wm.Seq)

			// Hydration on another node rejects it as well and installs nothing.
			other := e.secondNode(t, sqlstore.SQLite)
			err = other.sy.EnsureFresh(other.ctx, other.pbRepo(localRelPath))
			require.Equal(t, codes.Internal, status.Code(err), "error: %v", err)
			_, err = os.Stat(other.localPath())
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestSyncerAppliedReportsNoWatermarkForUnsyncedRepository(t *testing.T) {
	e := newEnv(t, envOptions{})

	_, ok, err := e.sy.Applied(e.ctx, e.pbRepo("never-synced.git"))
	require.NoError(t, err)
	require.False(t, ok)
}
