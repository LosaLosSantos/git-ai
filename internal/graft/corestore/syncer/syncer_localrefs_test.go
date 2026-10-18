package syncer_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/graft/corestore/syncer"
)

// markers returns the dirty markers of the node's storage.
func (e *env) markers(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(e.storagePath, localrefs.MarkerDirName))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// untrusted returns graft_storage_ref_untrusted_total{reason} of the node.
func (e *env) untrusted(t *testing.T, reason string) float64 {
	t.Helper()

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(e.core.RefDurability))
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "graft_storage_ref_untrusted_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "reason" && label.GetValue() == reason {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// tearNewestTable truncates the newest table of the local repository's stack:
// what a kernel restart can leave of a table written without fsync.
func (e *env) tearNewestTable(t *testing.T) {
	t.Helper()

	list, err := os.ReadFile(filepath.Join(e.localPath(), "reftable", "tables.list"))
	require.NoError(t, err)
	names := strings.Fields(string(list))
	require.NotEmpty(t, names)
	require.NoError(t, os.Truncate(filepath.Join(e.localPath(), "reftable", names[len(names)-1]), 0))
}

// caughtUpDirty returns a node whose local repository was hydrated, then caught
// up with one changeset: its references were written without fsync, so the
// repository is dirty.
func caughtUpDirty(t *testing.T, opts ...localrefs.DurabilityOption) (*env, corestore.Repository, git.ObjectID) {
	t.Helper()

	e := newEnv(t, envOptions{})
	coretest.EnableLocalRefs(t, e.core, e.locator, opts...)
	src := e.newSource(t)
	c1 := e.writeCommit(t, src)
	repo := e.register(t, e.key(localRelPath), src, c1)

	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Empty(t, e.markers(t), "a hydration writes its references durably")

	c2 := e.advanceMain(t, repo.ID, src, c1)
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Equal(t, float64(1), syncCount(t, e.sy, "caught_up"))
	require.Len(t, e.markers(t), 1, "the catch-up marks the repository before writing without fsync")

	repo, err := e.core.Meta.GetRepository(e.ctx, repo.Key)
	require.NoError(t, err)
	return e, repo, c2
}

func TestSyncerCatchUpMarksTheRepositoryDirty(t *testing.T) {
	t.Parallel()

	e, repo, _ := caughtUpDirty(t)
	e.verifySynced(t, repo)
	require.Equal(t, []string{e.localPath()}, e.core.RefDurability.Dirty())

	e.sy.DurableStep(e.ctx, e.localPath())
	require.Empty(t, e.markers(t))
	require.Empty(t, e.core.RefDurability.Dirty())
	e.verifySynced(t, repo)
}

// TestSyncerMovesAsideARepositoryDirtyAtAKernelRestart is the recovery of the
// LOCAL REFERENCE WRITES RULE: a repository with references written without
// fsync when the kernel restarted is moved aside before anything reads it, and
// hydrated again. Without the recovery the torn stack reads as an empty
// repository, without an error.
func TestSyncerMovesAsideARepositoryDirtyAtAKernelRestart(t *testing.T) {
	t.Parallel()

	e, repo, _ := caughtUpDirty(t, localrefs.WithBootID("boot-a"))
	e.tearNewestTable(t)

	// A read before the recovery returns the wrong state, and nothing reports
	// an error: git lists no references.
	torn := gittest.Exec(t, e.cfg, "-C", e.localPath(), "for-each-ref", "--format=%(refname)")
	require.Empty(t, strings.TrimSpace(string(torn)))

	// The node restarts after a kernel restart.
	coretest.EnableLocalRefs(t, e.core, e.locator, localrefs.WithBootID("boot-b"))
	e.sy = syncer.New(e.core, e.logger, e.locator, e.repoFactory, e.gitCmdFactory)
	require.NoError(t, e.sy.RecoverLocalReferences(e.ctx, []string{e.storageName}))

	require.NoDirExists(t, e.localPath())
	require.Empty(t, e.markers(t))
	require.Equal(t, float64(1), e.untrusted(t, localrefs.ReasonReboot))

	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Equal(t, float64(1), syncCount(t, e.sy, "hydrated"), "the restarted node hydrates the repository moved aside")
	e.verifySynced(t, repo)
}

// TestSyncerKeepsARepositoryDirtyAtAProcessRestart verifies that a restart of
// the process alone keeps the repository: the kernel kept every write.
func TestSyncerKeepsARepositoryDirtyAtAProcessRestart(t *testing.T) {
	t.Parallel()

	e, repo, _ := caughtUpDirty(t, localrefs.WithBootID("boot-a"))

	coretest.EnableLocalRefs(t, e.core, e.locator, localrefs.WithBootID("boot-a"))
	e.sy = syncer.New(e.core, e.logger, e.locator, e.repoFactory, e.gitCmdFactory)
	require.NoError(t, e.sy.RecoverLocalReferences(e.ctx, []string{e.storageName}))

	require.DirExists(t, e.localPath())
	require.Equal(t, []string{e.localPath()}, e.core.RefDurability.Dirty())
	e.verifySynced(t, repo)

	e.sy.DurableStep(e.ctx, e.localPath())
	require.Empty(t, e.markers(t))
}

// TestSyncerRecoveryKeepsWhatIsNotACache verifies that the recovery never moves
// a local repository that a hydration cannot rebuild: one the database does
// not hold. Its marker is dropped.
func TestSyncerRecoveryKeepsWhatIsNotACache(t *testing.T) {
	t.Parallel()

	e := newEnv(t, envOptions{})
	coretest.EnableLocalRefs(t, e.core, e.locator, localrefs.WithBootID("boot-a"))
	pb, path := gittest.CreateRepository(t, e.ctx, e.cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
		ReferenceBackend:       git.ReferenceBackendReftables,
	})
	require.NoError(t, e.core.RefDurability.MarkDirty(e.ctx, localrefs.RepoRef{Storage: pb.GetStorageName(), RelativePath: pb.GetRelativePath()}, path))

	coretest.EnableLocalRefs(t, e.core, e.locator, localrefs.WithBootID("boot-b"))
	e.sy = syncer.New(e.core, e.logger, e.locator, e.repoFactory, e.gitCmdFactory)
	require.NoError(t, e.sy.RecoverLocalReferences(e.ctx, []string{e.storageName}))

	require.DirExists(t, path)
	require.Empty(t, e.markers(t))
	require.Equal(t, float64(0), e.untrusted(t, localrefs.ReasonReboot))
}

// TestSyncerDurableStepMovesAsideOnAFailedWrite verifies the runtime half of
// the rule: when the kernel reports that a write of the references failed, the
// repository is moved aside at once, without waiting for a restart, and the
// next request hydrates it.
func TestSyncerDurableStepMovesAsideOnAFailedWrite(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool
	e, repo, _ := caughtUpDirty(t, localrefs.WithSync(func(f *os.File) error {
		if failing.Load() {
			return errors.New("input/output error")
		}
		return f.Sync()
	}))

	failing.Store(true)
	e.sy.DurableStep(e.ctx, e.localPath())
	failing.Store(false)

	require.NoDirExists(t, e.localPath())
	require.Empty(t, e.markers(t))
	require.Equal(t, float64(1), e.untrusted(t, localrefs.ReasonSyncFailed))

	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Equal(t, float64(2), syncCount(t, e.sy, "hydrated"), "the first hydration and the one after the move")
	e.verifySynced(t, repo)
}
