package e2e

import (
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/sdk"
)

// TestStorageShadowModeBenchmarkHasNoDivergence runs the mixed landing load of
// the benchmark (private changes and changes that conflict in one file, all
// on one branch, batched as configured by default) against a server in shadow
// mode, on a repository that had history before it was imported. Every write
// is mirrored into the database without a single divergence or shadow error,
// and the database ends with exactly the references git has.
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
