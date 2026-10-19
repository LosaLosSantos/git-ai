package e2e

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/sdk"
)

// TestStorageLandIdentity lands a change by rebase on one node of the storage core in its
// authoritative mode and lands it again on another node, as a client does whose response was lost
// and whose retry reached another node: the land identity in the database answers on every node
// (doc/graft/PLAN11.md, 4.1). A revert of the change on the branch does not let it land again.
func TestStorageLandIdentity(t *testing.T) {
	t.Parallel()

	for _, batched := range []bool{false, true} {
		t.Run(fmt.Sprintf("batched=%t", batched), func(t *testing.T) {
			t.Parallel()

			forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				graft, tokens := workerGraft(2)
				if !batched {
					graft = unbatched(graft)
				}
				cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, graft)
				a := cluster.addNode(t)
				b := cluster.addNode(t)

				rel := a.createRepository(t).GetRelativePath()
				onA := a.agent(t, rel, tokens[workerName(0)])
				onB := b.agent(t, rel, tokens[workerName(1)])

				base, err := commitFile(a.ctx, onA, workerName(0), "main", "", "README.md", "land identity\n")
				require.NoError(t, err)
				_, err = commitFile(a.ctx, onA, workerName(0), "main", base, "moved.txt", "main moved on\n")
				require.NoError(t, err)
				change, err := commitFile(a.ctx, onA, workerName(1), "work/change", base, "change.txt", "the change\n")
				require.NoError(t, err)

				first, err := landOn(a.ctx, onA, "main", change, false)
				require.NoError(t, err)
				require.True(t, first.Rewritten)

				again, err := landOn(b.ctx, onB, "main", change, false)
				require.NoError(t, err)
				require.True(t, again.AlreadyLanded, "node-b answers from the record of node-a's land")
				require.Equal(t, first.LandedCommitID, again.LandedCommitID)
				require.Empty(t, again.RevertedByCommitID)

				// The record names the changelog entry of the first land.
				var landedAt uint64
				for _, c := range allChangesets(a.ctx, t, onA) {
					if c.GetNewOid() == first.LandedCommitID {
						landedAt = c.GetSequence()
					}
				}
				require.NotZero(t, landedAt)
				require.Equal(t, landedAt, again.LandedSequence)
				require.Equal(t, fmt.Sprintf("landed as %s at changelog %d", first.LandedCommitID, landedAt), again.AlreadyLandedDetail)

				// A revert of the change on main, as graft-verify writes it.
				revert, err := onA.CommitFiles(a.ctx, sdk.CommitOptions{
					Branch:   "main",
					StartRev: first.LandedCommitID,
					Message:  fmt.Sprintf("graft-verify: revert\n\nThis reverts commit %s, which failed the verification of main.\n", first.LandedCommitID),
					Author:   sdk.Signature{Name: workerName(0), Email: workerName(0) + "@example.com"},
					User:     sdk.User{ID: workerName(0), Name: workerName(0), Email: workerName(0) + "@example.com"},
					Actions:  []sdk.Action{sdk.Delete{Path: "change.txt"}},
				})
				require.NoError(t, err)

				afterRevert, err := landOn(b.ctx, onB, "main", change, false)
				require.NoError(t, err)
				require.True(t, afterRevert.AlreadyLanded)
				require.Equal(t, first.LandedCommitID, afterRevert.LandedCommitID)
				require.Equal(t, revert.CommitID, afterRevert.RevertedByCommitID)
				require.Equal(t, fmt.Sprintf("landed as %s at changelog %d; it was reverted later by %s", first.LandedCommitID, landedAt, revert.CommitID), afterRevert.AlreadyLandedDetail)

				tip, err := onB.ResolveRevision(b.ctx, "main")
				require.NoError(t, err)
				require.Equal(t, revert.CommitID, tip, "the revert stays")

				a.requireConsistent(t, rel)
				b.requireConsistent(t, rel)
			})
		})
	}
}
