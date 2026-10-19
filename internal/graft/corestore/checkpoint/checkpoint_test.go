package checkpoint_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/checkpoint"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/syncer"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

// relPath is the relative path of the repository the tests checkpoint.
const relPath = "repo.git"

func forEachDialect(t *testing.T, test func(t *testing.T, dialect sqlstore.Dialect)) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			test(t, dialect)
		})
	}
}

// env is one node: a storage whose local repositories are caches, a storage
// core, its syncer and a checkpointer.
type env struct {
	ctx         context.Context
	cfg         config.Cfg
	logger      log.Logger
	locator     storage.Locator
	repoFactory localrepo.Factory
	core        *corestore.Core
	sy          *syncer.Syncer
	cp          *checkpoint.Checkpointer
	databaseURL string
	bucketURL   string
}

type envOptions struct {
	dialect     sqlstore.Dialect
	mode        corestore.Mode
	databaseURL string
	bucketURL   string
	gcGrace     time.Duration
	gcMargin    time.Duration
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
		GCGrace:     opts.gcGrace,
		GCMargin:    opts.gcMargin,
	})
	sy := syncer.New(core, logger, locator, repoFactory, gitCmdFactory)

	return &env{
		ctx:         ctx,
		cfg:         cfg,
		logger:      logger,
		locator:     locator,
		repoFactory: repoFactory,
		core:        core,
		sy:          sy,
		cp:          checkpoint.New(core, logger, locator, repoFactory, sy),
		databaseURL: opts.databaseURL,
		bucketURL:   opts.bucketURL,
	}
}

// secondNode returns another node sharing the database and the bucket of e.
func (e *env) secondNode(t *testing.T, dialect sqlstore.Dialect) *env {
	t.Helper()
	return newEnv(t, envOptions{dialect: dialect, databaseURL: e.databaseURL, bucketURL: e.bucketURL})
}

func (e *env) pbRepo() *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: e.cfg.Storages[0].Name, RelativePath: relPath}
}

func (e *env) key() corestore.RepoKey {
	return corestore.KeyOf(e.pbRepo())
}

// source is the repository the test commits are written to. Its references
// stay empty, so that the pack of a changeset holds the whole history of its
// tips.
type source struct {
	repo *localrepo.Repo
	path string
}

func (e *env) newSource(t *testing.T) source {
	t.Helper()

	pb, path := gittest.CreateRepository(t, e.ctx, e.cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	return source{repo: localrepo.NewTestRepo(t, e.cfg, pb), path: path}
}

func (e *env) commit(t *testing.T, src source, message string, parents ...git.ObjectID) git.ObjectID {
	t.Helper()
	return gittest.WriteCommit(t, e.cfg, src.path, gittest.WithMessage(message), gittest.WithParents(parents...))
}

// changeset uploads the pack of the tips (if any) and commits the updates as
// the next changeset of repository id.
func (e *env) changeset(t *testing.T, id int64, src source, updates []corestore.RefUpdate, tips ...git.ObjectID) corestore.CommitResult {
	t.Helper()

	pack, err := gitops.BuildPack(e.ctx, src.repo, tips, t.TempDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, pack.Remove()) }()

	in := corestore.ChangesetInput{
		RepoID:    id,
		Updates:   updates,
		Operation: "test",
		Actor:     "tester",
		NodeID:    "node-a",
	}
	if !pack.Empty() {
		in.PackKey = corestore.NewPackKey(id, pack.SHA256)
		in.PackDeadline = time.Now().Add(time.Hour)
		rc, err := pack.Open()
		require.NoError(t, err)
		require.NoError(t, e.core.Blobs.Put(e.ctx, in.PackKey, rc))
		require.NoError(t, rc.Close())
		in.PackBytes = pack.Size
	}

	res, err := e.core.Meta.CommitChangeset(e.ctx, in)
	require.NoError(t, err)
	require.False(t, res.NoOp)
	return res
}

// register records the repository with a first changeset that points main
// (and HEAD) at head.
func (e *env) register(t *testing.T, src source, head git.ObjectID) corestore.Repository {
	t.Helper()

	repo, _, err := e.core.Meta.RegisterRepository(e.ctx, e.key(), gittest.DefaultObjectHash.Format, corestore.InitialState{})
	require.NoError(t, err)

	e.changeset(t, repo.ID, src, []corestore.RefUpdate{
		{Name: "HEAD", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")},
		{Name: "refs/heads/main", Expect: corestore.ExpectValue, New: corestore.Target(head)},
	}, head)
	return repo
}

// setRef commits a changeset moving one reference from old to new (absent
// when empty), with the pack of new.
func (e *env) setRef(t *testing.T, id int64, src source, name string, old, new git.ObjectID) uint64 {
	t.Helper()

	var tips []git.ObjectID
	if new != "" {
		tips = append(tips, new)
	}
	return e.changeset(t, id, src, []corestore.RefUpdate{
		{Name: name, Expect: corestore.ExpectValue, Old: corestore.Target(old), New: corestore.Target(new)},
	}, tips...).Seq
}

func (e *env) head(t *testing.T) uint64 {
	t.Helper()

	r, err := e.core.Meta.GetRepository(e.ctx, e.key())
	require.NoError(t, err)
	return r.HeadSeq
}

func (e *env) watermark(t *testing.T) corestore.Watermark {
	t.Helper()

	wm, ok, err := e.sy.Applied(e.ctx, e.pbRepo())
	require.NoError(t, err)
	require.True(t, ok)
	return wm
}

// packKeys returns the pack objects of the repository in the bucket.
func (e *env) packKeys(t *testing.T, id int64) map[string]struct{} {
	t.Helper()

	keys := map[string]struct{}{}
	require.NoError(t, e.core.Blobs.List(e.ctx, fmt.Sprintf("repos/%d/packs/", id), func(info corestore.BlobInfo) error {
		keys[info.Key] = struct{}{}
		return nil
	}))
	return keys
}

// referenced reports whether a changeset or a checkpoint references the key.
func (e *env) referenced(t *testing.T, id int64, key string) bool {
	t.Helper()

	state, err := e.core.Meta.GCSnapshot(e.ctx, id)
	require.NoError(t, err)
	if _, ok := state.Packs[key]; ok {
		return true
	}
	for _, cp := range state.Checkpoints {
		if cp.PackKey == key {
			return true
		}
	}
	return false
}

// newKeys returns the keys of after that are not in before.
func newKeys(before, after map[string]struct{}) []string {
	var keys []string
	for key := range after {
		if _, ok := before[key]; !ok {
			keys = append(keys, key)
		}
	}
	return keys
}

// restore builds a repository from the checkpoint alone: its pack indexed into
// an empty repository and its snapshot applied. The repository must be
// connected, with exactly the snapshot as references. It returns its path.
func (e *env) restore(t *testing.T, cp corestore.Checkpoint) string {
	t.Helper()

	rc, err := e.core.Blobs.Get(e.ctx, cp.PackKey)
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.EqualValues(t, len(data), cp.PackBytes)

	pb, path := gittest.CreateRepository(t, e.ctx, e.cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, e.cfg, pb)
	require.NoError(t, gitops.IndexPack(e.ctx, repo, bytes.NewReader(data)))

	refs, err := e.core.Meta.CheckpointRefs(e.ctx, cp.RepoID, cp.Seq)
	require.NoError(t, err)
	require.NoError(t, gitops.ApplyRefs(e.ctx, repo, refs))
	restored, err := gitops.ReadRefs(e.ctx, repo)
	require.NoError(t, err)
	require.Equal(t, refs, restored)

	gittest.Exec(t, e.cfg, "-C", path, "fsck", "--connectivity-only")
	return path
}

func hasObject(t *testing.T, cfg config.Cfg, path string, oid git.ObjectID) bool {
	t.Helper()
	return gittest.NewCommand(t, cfg, "-C", path, "cat-file", "-e", oid.String()).Run() == nil
}

func TestCreate(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.commit(t, src, "c1")
		repo := e.register(t, src, c1)
		c2 := e.commit(t, src, "c2", c1)
		e.setRef(t, repo.ID, src, "refs/heads/main", c1, c2)
		other := e.commit(t, src, "other", c1)
		e.setRef(t, repo.ID, src, "refs/heads/other", "", other)

		cp, created, err := e.cp.Create(e.ctx, e.pbRepo())
		require.NoError(t, err)
		require.True(t, created)
		require.Equal(t, e.head(t), cp.Seq)
		require.Equal(t, repo.ID, cp.RepoID)
		require.True(t, corestore.IsOwnedPackKey(cp.PackKey))
		keyRepo, _, ok := corestore.ParsePackKey(cp.PackKey)
		require.True(t, ok)
		require.Equal(t, repo.ID, keyRepo)

		newest, ok, err := e.core.Meta.NewestCheckpoint(e.ctx, repo.ID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, cp, newest)

		dbRefs, _, err := e.core.Meta.Refs(e.ctx, repo.ID)
		require.NoError(t, err)
		snapshot, err := e.core.Meta.CheckpointRefs(e.ctx, repo.ID, cp.Seq)
		require.NoError(t, err)
		require.Equal(t, dbRefs, snapshot)
		e.restore(t, cp)

		// A second build at the same head returns the checkpoint and uploads
		// nothing.
		before := e.packKeys(t, repo.ID)
		again, created, err := e.cp.Create(e.ctx, e.pbRepo())
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, cp, again)
		require.Equal(t, before, e.packKeys(t, repo.ID))

		// A later head gets a new checkpoint.
		c3 := e.commit(t, src, "c3", c2)
		e.setRef(t, repo.ID, src, "refs/heads/main", c2, c3)
		next, created, err := e.cp.Create(e.ctx, e.pbRepo())
		require.NoError(t, err)
		require.True(t, created)
		require.Greater(t, next.Seq, cp.Seq)
		require.NotEqual(t, cp.PackKey, next.PackKey)
		e.restore(t, next)
	})
}

// TestCreateKeepsKeepAroundCommits: the commits of open reviews are kept by
// their keep-around references (REVIEW ROOTS RULE), which the checkpoint
// closure covers like every reference; a commit whose reference was swept is
// not kept.
func TestCreateKeepsKeepAroundCommits(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		src := e.newSource(t)
		c1 := e.commit(t, src, "c1")
		repo := e.register(t, src, c1)

		kept := e.commit(t, src, "open review", c1)
		e.setRef(t, repo.ID, src, corestore.KeepAroundRef(kept.String()), "", kept)
		swept := e.commit(t, src, "closed review", c1)
		e.setRef(t, repo.ID, src, corestore.KeepAroundRef(swept.String()), "", swept)
		e.setRef(t, repo.ID, src, corestore.KeepAroundRef(swept.String()), swept, "")

		cp, created, err := e.cp.Create(e.ctx, e.pbRepo())
		require.NoError(t, err)
		require.True(t, created)

		path := e.restore(t, cp)
		require.True(t, hasObject(t, e.cfg, path, kept))
		require.False(t, hasObject(t, e.cfg, path, swept))
	})
}

// TestCreateOnALaggingCopy is change D: the build catches the local copy up to
// the head before it packs.
func TestCreateOnALaggingCopy(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		a := newEnv(t, envOptions{dialect: dialect})
		b := a.secondNode(t, dialect)
		src := a.newSource(t)
		c1 := a.commit(t, src, "c1")
		repo := a.register(t, src, c1)

		require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo()))
		require.Equal(t, corestore.Watermark{RepoID: repo.ID, Seq: 1}, b.watermark(t))

		c2 := a.commit(t, src, "c2", c1)
		a.setRef(t, repo.ID, src, "refs/heads/main", c1, c2)
		c3 := a.commit(t, src, "c3", c2)
		a.setRef(t, repo.ID, src, "refs/heads/main", c2, c3)

		cp, created, err := b.cp.Create(b.ctx, b.pbRepo())
		require.NoError(t, err)
		require.True(t, created)
		require.Equal(t, uint64(3), cp.Seq)
		require.Equal(t, corestore.Watermark{RepoID: repo.ID, Seq: 3}, b.watermark(t))
		path := b.restore(t, cp)
		require.True(t, hasObject(t, b.cfg, path, c3))
	})
}

func TestCreateCrashPoints(t *testing.T) {
	errCrash := errors.New("crash")

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, point := range []string{checkpoint.PointAfterUpload, checkpoint.PointAfterCommit} {
			t.Run(point, func(t *testing.T) {
				e := newEnv(t, envOptions{dialect: dialect})
				src := e.newSource(t)
				c1 := e.commit(t, src, "c1")
				repo := e.register(t, src, c1)
				before := e.packKeys(t, repo.ID)

				checkpoint.SetFaultHook(e.cp, func(p string) error {
					if p == point {
						return errCrash
					}
					return nil
				})
				_, _, err := e.cp.Create(e.ctx, e.pbRepo())
				require.ErrorIs(t, err, errCrash)
				uploaded := newKeys(before, e.packKeys(t, repo.ID))
				newest, recorded, err := e.core.Meta.NewestCheckpoint(e.ctx, repo.ID)
				require.NoError(t, err)

				switch point {
				case checkpoint.PointAfterUpload:
					require.Len(t, uploaded, 1)
					require.False(t, e.referenced(t, repo.ID, uploaded[0]), "an orphan")
					require.False(t, recorded)
				case checkpoint.PointAfterCommit:
					require.True(t, recorded)
					require.Equal(t, []string{newest.PackKey}, uploaded)
				}

				// The re-run converges.
				checkpoint.SetFaultHook(e.cp, nil)
				cp, created, err := e.cp.Create(e.ctx, e.pbRepo())
				require.NoError(t, err)
				require.Equal(t, point != checkpoint.PointAfterCommit, created)
				if !created {
					require.Equal(t, newest, cp)
				}
				e.restore(t, cp)
			})
		}
	})
}

// slowBlobs is a bucket slower than the write deadline.
type slowBlobs struct {
	corestore.BlobStore
	// late stores the object and returns only after the deadline; otherwise
	// Put waits for the end of its context and stores nothing.
	late bool
}

func (b slowBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	if !b.late {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := b.BlobStore.Put(context.WithoutCancel(ctx), key, r); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// TestCreateWriteDeadline: past the write deadline the upload is cancelled or
// the commit is not started, the build fails as a storage failure, and no row
// references the pack.
func TestCreateWriteDeadline(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, late := range []bool{false, true} {
			t.Run(fmt.Sprintf("late=%t", late), func(t *testing.T) {
				// The write deadline is gc_grace - gc_margin = 2s.
				e := newEnv(t, envOptions{dialect: dialect, gcGrace: 3 * time.Second, gcMargin: time.Second})
				src := e.newSource(t)
				c1 := e.commit(t, src, "c1")
				repo := e.register(t, src, c1)
				before := e.packKeys(t, repo.ID)
				e.core.Blobs = slowBlobs{BlobStore: e.core.Blobs, late: late}

				_, _, err := e.cp.Create(e.ctx, e.pbRepo())
				require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
				require.ErrorIs(t, err, corestore.ErrPackDeadline)

				_, ok, err := e.core.Meta.NewestCheckpoint(e.ctx, repo.ID)
				require.NoError(t, err)
				require.False(t, ok)
				uploaded := newKeys(before, e.packKeys(t, repo.ID))
				if late {
					require.Len(t, uploaded, 1)
					require.False(t, e.referenced(t, repo.ID, uploaded[0]))
				} else {
					require.Empty(t, uploaded)
				}
			})
		}
	})
}

func TestCreateWithoutObjects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		e := newEnv(t, envOptions{dialect: dialect})
		repo, _, err := e.core.Meta.RegisterRepository(e.ctx, e.key(), gittest.DefaultObjectHash.Format, corestore.InitialState{})
		require.NoError(t, err)
		_, err = e.core.Meta.CommitChangeset(e.ctx, corestore.ChangesetInput{
			RepoID: repo.ID,
			Updates: []corestore.RefUpdate{
				{Name: "HEAD", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")},
			},
			Operation: "test",
			Actor:     "tester",
			NodeID:    "node-a",
		})
		require.NoError(t, err)

		cp, created, err := e.cp.Create(e.ctx, e.pbRepo())
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, corestore.Checkpoint{}, cp)
	})
}

func TestCreatePreconditions(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		t.Run("shadow mode", func(t *testing.T) {
			e := newEnv(t, envOptions{dialect: dialect, mode: corestore.ModeShadow})
			_, _, err := e.cp.Create(e.ctx, e.pbRepo())
			require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
		})

		t.Run("untracked", func(t *testing.T) {
			e := newEnv(t, envOptions{dialect: dialect})
			_, _, err := e.cp.Create(e.ctx, e.pbRepo())
			require.Equal(t, codes.NotFound, status.Code(err), "%v", err)
		})

		t.Run("incomplete import", func(t *testing.T) {
			e := newEnv(t, envOptions{dialect: dialect})
			_, _, err := e.core.Meta.RegisterRepository(e.ctx, e.key(), gittest.DefaultObjectHash.Format, corestore.InitialState{})
			require.NoError(t, err)
			_, _, err = e.cp.Create(e.ctx, e.pbRepo())
			require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
		})

		// A Freshener that does not bring the local repository up to date
		// breaks its contract: the build refuses to pack from it.
		t.Run("freshener contract", func(t *testing.T) {
			e := newEnv(t, envOptions{dialect: dialect})
			src := e.newSource(t)
			e.register(t, src, e.commit(t, src, "c1"))
			cp := checkpoint.New(e.core, e.logger, e.locator, e.repoFactory, corestore.NopFreshener{})
			_, _, err := cp.Create(e.ctx, e.pbRepo())
			require.Equal(t, codes.Internal, status.Code(err), "%v", err)
		})
	})
}
