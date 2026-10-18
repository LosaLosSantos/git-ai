package e2e

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/sdk"
)

// overrunBlobs is a bucket whose uploads, while on is set, last until the end
// of their context: the Preparing phase of a shadow transaction then uses up
// the whole shadow budget.
type overrunBlobs struct {
	corestore.BlobStore
	on *atomic.Bool
}

func (b overrunBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	if b.on.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return b.BlobStore.Put(ctx, key, r)
}

// overrunMeta is a database whose changeset commits, while on is set, last
// until the end of their context: the Prepared phase of a shadow transaction
// then runs past the shadow budget.
type overrunMeta struct {
	corestore.MetaStore
	on *atomic.Bool
}

func (m overrunMeta) CommitChangeset(ctx context.Context, in corestore.ChangesetInput) (corestore.CommitResult, error) {
	if m.on.Load() {
		<-ctx.Done()
		return corestore.CommitResult{}, ctx.Err()
	}
	return m.MetaStore.CommitChangeset(ctx, in)
}

// TestStorageShadowModeBudgetOverrun checks what RULE 4 guarantees for a shadow
// transaction whose storage-core work overruns shadow_timeout
// (doc/graft/PLAN7.md, SHADOW BUDGET requirement): the git write succeeds; the
// failure is logged at error level (and counted, see the voter's unit tests);
// nothing is recorded; VerifyRepository reports the reference as diverging;
// and the next write of the reference is recorded as a forced changeset and
// logged as a divergence, after which the database follows git again. Both
// phases are covered: an overrun in Preparing (the upload of the pack), which
// leaves Prepared no budget at all, and an overrun in Prepared (the database
// commit, while git holds the reference lock).
func TestStorageShadowModeBudgetOverrun(t *testing.T) {
	t.Parallel()

	for _, phase := range []string{"preparing", "prepared"} {
		t.Run(phase, func(t *testing.T) {
			forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				const ref = "refs/heads/budget"
				who := workerName(0)

				graft, tokens := workerGraft(1)
				logger := testhelper.NewLogger(t)
				logs := testhelper.AddLoggerHook(logger)
				cluster := newStorageCluster(t, dialect, corestore.ModeShadow, graft)
				cluster.logger = logger

				var overrun atomic.Bool
				cluster.adjust = func(core *corestore.Core) {
					if phase == "preparing" {
						core.Blobs = overrunBlobs{BlobStore: core.Blobs, on: &overrun}
					} else {
						core.Meta = overrunMeta{MetaStore: core.Meta, on: &overrun}
					}
				}
				node := cluster.addNode(t)
				ctx := node.ctx

				rel := node.createRepository(t).GetRelativePath()
				repo := node.agent(t, rel, tokens[who])
				base, err := commitFile(ctx, repo, who, "main", "", "a.txt", "a")
				require.NoError(t, err)
				_, err = node.operator(t, rel).StorageImport(ctx)
				require.NoError(t, err)

				// A write whose shadow work overruns the budget: git commits it.
				overrun.Store(true)
				first, err := commitFile(ctx, repo, who, "budget", base, "b.txt", "b")
				overrun.Store(false)
				require.NoError(t, err, "shadow mode never fails the git write")

				require.Equal(t, corestore.Target(first), node.localRefs(t, rel)[ref])
				require.NotContains(t, node.dbRefs(t, rel), ref, "the overrun write is not recorded")
				require.True(t, hasLog(logs.AllEntries(), logrus.ErrorLevel, "shadow write failed, the git write proceeds"),
					"the overrun is logged at error level")

				verified, err := node.operator(t, rel).StorageVerify(ctx, false)
				require.NoError(t, err)
				require.Len(t, verified.Divergences, 1)
				require.Equal(t, ref, verified.Divergences[0].Reference)
				require.Equal(t, first, verified.Divergences[0].LocalTarget)
				require.Empty(t, verified.Divergences[0].DatabaseTarget)

				// The next write of the reference expects the value git has, which the
				// database does not: it is recorded as forced and counted as a
				// divergence, and the database follows git again.
				second, err := commitFile(ctx, repo, who, "budget", first, "c.txt", "c")
				require.NoError(t, err)

				rec := node.record(t, rel)
				changesets, err := node.core.Meta.ListChangesets(ctx, rec.ID, rec.HeadSeq-1, rec.HeadSeq)
				require.NoError(t, err)
				require.Len(t, changesets, 1)
				require.True(t, changesets[0].Forced, "the next write of the reference is a forced changeset")
				require.Equal(t, corestore.Target(second), changesets[0].Updates[0].New)
				require.True(t, hasLog(logs.AllEntries(), logrus.WarnLevel, "diverges from the database"))

				require.Equal(t, node.localRefs(t, rel), node.dbRefs(t, rel))
				verified, err = node.operator(t, rel).StorageVerify(ctx, true)
				require.NoError(t, err)
				require.Empty(t, verified.Divergences)
				require.True(t, verified.ConnectivityOK, verified.ConnectivityError)
			})
		})
	}
}

// hasLog reports whether an entry at level has a message containing text.
func hasLog(entries []*logrus.Entry, level logrus.Level, text string) bool {
	for _, entry := range entries {
		if entry.Level == level && strings.Contains(entry.Message, text) {
			return true
		}
	}
	return false
}

// TestStorageShadowModeBenchmarkHasNoDivergence runs the mixed landing load of
// the benchmark (private changes and changes that conflict in one file, all
// on one branch, batched as configured by default) against a server in shadow
// mode, on a repository that had history before it was imported. Every write
// is mirrored into the database without a single divergence or shadow error,
// and the database ends with exactly the references git has.
//
// It tests the correctness of the mirroring, so the shadow budget is made
// non-binding: shadow mode records a transaction only if its storage-core work
// fits shadow_timeout (doc/graft/PLAN7.md, SHADOW BUDGET requirement), and
// whether the work of this load fits 5 seconds depends on how loaded the
// machine is, not on the code. The budget path itself (a write past the budget
// is counted, logged and not recorded, and the next write of its reference is
// a forced changeset) is TestStorageShadowModeBudgetOverrun.
func TestStorageShadowModeBenchmarkHasNoDivergence(t *testing.T) {
	t.Parallel()

	forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		const private, shared = 40, 10
		const branch = "bench-shadow"

		graft, tokens := workerGraft(private + shared)
		logger := testhelper.NewLogger(t)
		logs := testhelper.AddLoggerHook(logger)
		cluster := newStorageCluster(t, dialect, corestore.ModeShadow, graft)
		cluster.logger = logger
		cluster.shadowTimeout = time.Minute
		node := cluster.addNode(t)
		ctx := node.ctx

		rel := node.createRepository(t).GetRelativePath()
		repos := make([]*sdk.Repo, private+shared)
		for i := range repos {
			repos[i] = node.agent(t, rel, tokens[workerName(i)])
		}

		// History from before the import: in shadow mode the writes of an
		// untracked repository are skipped.
		base, err := commitFile(ctx, repos[0], workerName(0), "main", "", sharedLogPath, sharedLogHeader)
		require.NoError(t, err)
		_, err = landAs(ctx, repos[0], workerName(0), branch, base)
		require.NoError(t, err)
		_, err = node.core.Meta.GetRepository(ctx, corestore.KeyOf(node.repository(rel)))
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound, "shadow mode does not register repositories")

		imported, err := node.operator(t, rel).StorageImport(ctx)
		require.NoError(t, err)
		require.Equal(t, uint64(1), imported.Sequence)
		require.Positive(t, imported.PackBytes)
		require.Equal(t, node.localRefs(t, rel), node.dbRefs(t, rel), "the import copies every reference")

		commits := make([]string, private+shared)
		for i := range commits {
			commits[i], err = sharedChange(ctx, repos[i], workerName(i), base, 1, i >= private)
			require.NoError(t, err)
		}

		tally := runLoad(private+shared, func(agent int) error {
			_, err := landOn(ctx, repos[agent], branch, commits[agent], false)
			return err
		})
		require.NoError(t, tally.check(private+shared))
		require.Equal(t, expectedMixedOutcome(private, shared), tally, "outcome of the mixed load")

		// Every reference transaction reached the database, none diverged.
		rec := node.record(t, rel)
		changesets, err := node.core.Meta.ListChangesets(ctx, rec.ID, 0, rec.HeadSeq)
		require.NoError(t, err)
		require.Len(t, changesets, int(rec.HeadSeq), "the sequences are dense")
		require.Greater(t, len(changesets), 1+private+shared, "the import, the commits and the lands are recorded")
		for _, cs := range changesets {
			require.False(t, cs.Forced, "changeset %d (%s by %s) is a divergence", cs.Seq, cs.Operation, cs.Actor)
		}
		for _, entry := range logs.AllEntries() {
			require.NotContains(t, entry.Message, "diverges from the database")
			require.False(t, strings.Contains(entry.Message, "storage core") && entry.Level <= logrus.ErrorLevel,
				"storage core error: %s %v", entry.Message, entry.Data)
		}

		require.Equal(t, node.localRefs(t, rel), node.dbRefs(t, rel), "the database has exactly the references of git")
		verified, err := node.operator(t, rel).StorageVerify(ctx, true)
		require.NoError(t, err)
		require.Empty(t, verified.Divergences)
		require.True(t, verified.ConnectivityOK, verified.ConnectivityError)
		require.Equal(t, rec.HeadSeq, verified.HeadSequence)
	})
}
