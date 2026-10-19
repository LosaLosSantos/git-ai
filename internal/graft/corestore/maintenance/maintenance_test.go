package maintenance_test

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/checkpoint"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gc"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/maintenance"
	"github.com/feder-cr/git-ai/internal/graft/corestore/syncer"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

const relPath = "repo.git"

// env is one node with a repository of the storage core: its database record,
// a source repository its packs are built from, and the node's syncer.
type env struct {
	t      *testing.T
	cfg    config.Cfg
	core   *corestore.Core
	sy     *syncer.Syncer
	runner *maintenance.Runner
	repo   corestore.Repository
	src    *localrepo.Repo
	srcDir string
	head   git.ObjectID
}

func newEnv(t *testing.T, opts coretest.Options, cfg maintenance.Config) *env {
	t.Helper()

	gcfg := testcfg.Build(t)
	ctx := testhelper.Context(t)
	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(gcfg)
	gitCmdFactory := gittest.NewCommandFactory(t, gcfg)
	cache := catfile.NewCache(gcfg)
	t.Cleanup(cache.Stop)
	repoFactory := localrepo.NewFactory(logger, locator, gitCmdFactory, cache)

	core := coretest.Open(t, opts)
	sy := syncer.New(core, logger, locator, repoFactory, gitCmdFactory)
	runner := maintenance.New(core, checkpoint.New(core, logger, locator, repoFactory, sy), gc.New(core, logger), sy, logger, cfg,
		[]string{gcfg.Storages[0].Name})

	srcPB, srcDir := gittest.CreateRepository(t, ctx, gcfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	e := &env{t: t, cfg: gcfg, core: core, sy: sy, runner: runner, src: localrepo.NewTestRepo(t, gcfg, srcPB), srcDir: srcDir}

	repo, _, err := core.Meta.RegisterRepository(ctx, e.key(), gittest.DefaultObjectHash.Format, corestore.InitialState{})
	require.NoError(t, err)
	e.repo = repo
	e.head = e.commit("c0")
	e.changeset([]corestore.RefUpdate{
		{Name: "HEAD", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")},
		{Name: "refs/heads/main", Expect: corestore.ExpectValue, New: corestore.Target(e.head)},
	}, e.head)
	return e
}

func (e *env) key() corestore.RepoKey { return corestore.KeyOf(e.pbRepo()) }

func (e *env) pbRepo() *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: e.cfg.Storages[0].Name, RelativePath: relPath}
}

func (e *env) commit(message string, parents ...git.ObjectID) git.ObjectID {
	return gittest.WriteCommit(e.t, e.cfg, e.srcDir, gittest.WithMessage(message), gittest.WithParents(parents...))
}

func (e *env) changeset(updates []corestore.RefUpdate, tips ...git.ObjectID) {
	e.t.Helper()
	ctx := testhelper.Context(e.t)

	pack, err := gitops.BuildPack(ctx, e.src, tips, e.t.TempDir())
	require.NoError(e.t, err)
	defer func() { require.NoError(e.t, pack.Remove()) }()

	in := corestore.ChangesetInput{RepoID: e.repo.ID, Updates: updates, Operation: "test", Actor: "tester", NodeID: "node-a"}
	if !pack.Empty() {
		in.PackKey = corestore.NewPackKey(e.repo.ID, pack.SHA256)
		rc, err := pack.Open()
		require.NoError(e.t, err)
		require.NoError(e.t, e.core.Blobs.Put(ctx, in.PackKey, rc))
		require.NoError(e.t, rc.Close())
		in.PackBytes = pack.Size
	}
	_, err = e.core.Meta.CommitChangeset(ctx, in)
	require.NoError(e.t, err)
}

// advance moves main by n commits, one changeset each.
func (e *env) advance(n int) {
	e.t.Helper()
	for i := 0; i < n; i++ {
		next := e.commit("next", e.head)
		e.changeset([]corestore.RefUpdate{{Name: "refs/heads/main", Expect: corestore.ExpectValue, Old: corestore.Target(e.head), New: corestore.Target(next)}}, next)
		e.head = next
	}
}

// fresh catches the node's local copy up.
func (e *env) fresh() {
	e.t.Helper()
	require.NoError(e.t, e.sy.EnsureFresh(testhelper.Context(e.t), e.pbRepo()))
}

func (e *env) newestCheckpoint() (corestore.Checkpoint, bool) {
	e.t.Helper()
	cp, ok, err := e.core.Meta.NewestCheckpoint(testhelper.Context(e.t), e.repo.ID)
	require.NoError(e.t, err)
	return cp, ok
}

func (e *env) headSeq() uint64 {
	e.t.Helper()
	r, err := e.core.Meta.GetRepository(testhelper.Context(e.t), e.key())
	require.NoError(e.t, err)
	return r.HeadSeq
}

// metric returns the value of the counter name with the label value (when
// not empty).
func (e *env) metric(name, label string) float64 {
	e.t.Helper()
	reg := prometheus.NewPedanticRegistry()
	require.NoError(e.t, reg.Register(e.runner))
	families, err := reg.Gather()
	require.NoError(e.t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.GetMetric() {
			if label == "" {
				return m.GetCounter().GetValue()
			}
			for _, l := range m.GetLabel() {
				if l.GetValue() == label {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func config3(changesets uint64, packBytes int64) maintenance.Config {
	return maintenance.Config{Interval: time.Hour, CheckpointChangesets: changesets, CheckpointPackBytes: packBytes, LeaseTTL: time.Minute}
}

func TestRunOnceCheckpointsAfterEnoughChangesets(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	e := newEnv(t, coretest.Options{}, config3(3, 0))
	e.fresh()

	require.NoError(t, e.runner.RunOnce(ctx))
	_, ok := e.newestCheckpoint()
	require.False(t, ok, "one changeset is below the threshold")

	e.advance(2)
	e.fresh()
	require.NoError(t, e.runner.RunOnce(ctx))
	cp, ok := e.newestCheckpoint()
	require.True(t, ok)
	require.Equal(t, e.headSeq(), cp.Seq)
	require.Equal(t, float64(1), e.metric("graft_storage_maintenance_checkpoints_total", ""))

	// Nothing follows the checkpoint: no new one.
	require.NoError(t, e.runner.RunOnce(ctx))
	require.Equal(t, float64(1), e.metric("graft_storage_maintenance_checkpoints_total", ""))
	require.Equal(t, float64(3), e.metric("graft_storage_maintenance_runs_total", "ok"))
}

func TestRunOnceCheckpointsAfterEnoughPackBytes(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	e := newEnv(t, coretest.Options{}, config3(0, 1))
	e.fresh()

	require.NoError(t, e.runner.RunOnce(ctx))
	cp, ok := e.newestCheckpoint()
	require.True(t, ok, "the first changeset's pack is over one byte")
	require.Equal(t, e.headSeq(), cp.Seq)

	off := newEnv(t, coretest.Options{}, config3(0, 0))
	off.fresh()
	require.NoError(t, off.runner.RunOnce(ctx))
	_, ok = off.newestCheckpoint()
	require.False(t, ok, "both thresholds 0 never checkpoint")
}

func TestRunOnceSkipsARepositoryWithoutALocalCopy(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	e := newEnv(t, coretest.Options{}, config3(1, 0))

	require.NoError(t, e.runner.RunOnce(ctx))
	_, ok := e.newestCheckpoint()
	require.False(t, ok)
	require.Equal(t, float64(1), e.metric("graft_storage_maintenance_skipped_total", "no_local_copy"))
}

func TestRunOnceSkipsARepositoryWhoseLeaseIsHeld(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	e := newEnv(t, coretest.Options{}, config3(1, 0))
	e.fresh()

	acquired, err := e.core.Meta.AcquireMaintenanceLease(ctx, e.repo.ID, "node-b/1", time.Hour)
	require.NoError(t, err)
	require.True(t, acquired)

	require.NoError(t, e.runner.RunOnce(ctx))
	_, ok := e.newestCheckpoint()
	require.False(t, ok)
	require.Equal(t, float64(1), e.metric("graft_storage_maintenance_skipped_total", "lease_held"))

	require.NoError(t, e.core.Meta.ReleaseMaintenanceLease(ctx, e.repo.ID, "node-b/1"))
	require.NoError(t, e.runner.RunOnce(ctx))
	_, ok = e.newestCheckpoint()
	require.True(t, ok)

	// The run released its lease: another holder takes it at once.
	acquired, err = e.core.Meta.AcquireMaintenanceLease(ctx, e.repo.ID, "node-b/1", time.Hour)
	require.NoError(t, err)
	require.True(t, acquired)
}

// TestRunOnceCollectsAnOrphanPack verifies that the runs collect the
// repository: an object under its pack prefix that no row references is
// deleted once a run observed it gc_grace earlier.
func TestRunOnceCollectsAnOrphanPack(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	e := newEnv(t, coretest.Options{GCGrace: 200 * time.Millisecond, GCMargin: 50 * time.Millisecond}, config3(0, 0))
	e.fresh()

	content := []byte("an orphan pack")
	key := corestore.NewPackKey(e.repo.ID, sha256.Sum256(content))
	require.NoError(t, e.core.Blobs.Put(ctx, key, bytes.NewReader(content)))

	// The first run observes it; a run gc_grace later deletes it.
	require.NoError(t, e.runner.RunOnce(ctx))
	exists, err := e.core.Blobs.Exists(ctx, key)
	require.NoError(t, err)
	require.True(t, exists, "observed, not yet deleted")

	require.Eventually(t, func() bool {
		require.NoError(t, e.runner.RunOnce(ctx))
		exists, err := e.core.Blobs.Exists(ctx, key)
		require.NoError(t, err)
		return !exists
	}, 30*time.Second, 100*time.Millisecond)
	require.Equal(t, float64(1), e.metric("graft_storage_maintenance_packs_deleted_total", ""))
	require.Equal(t, float64(len(content)), e.metric("graft_storage_maintenance_bytes_reclaimed_total", ""))
}

func TestStartRunsEveryIntervalUntilStopped(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	cfg := config3(1, 0)
	cfg.Interval = 20 * time.Millisecond
	e := newEnv(t, coretest.Options{}, cfg)
	e.fresh()

	stop := e.runner.Start(ctx)
	require.Eventually(t, func() bool {
		_, ok := e.newestCheckpoint()
		return ok
	}, 30*time.Second, 10*time.Millisecond)
	// stop returns once the loop has ended (a run in progress is cancelled).
	stop()
}
