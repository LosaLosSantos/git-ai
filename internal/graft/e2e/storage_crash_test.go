package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
)

// TestStorageCrashInjection makes the write path of node A die at its two
// fault points (doc/graft/PLAN6.md, "Proof and docs"):
//
//   - after the pack upload and before the database commit: the bucket holds
//     an orphan pack, the database has no changeset, nothing is visible on any
//     node, and the write can simply be retried;
//   - after the database commit and before the git commit: the database has
//     the write, node A's local copy is behind, every other node serves the
//     write, and the next request on A repairs A's copy (EnsureFresh).
//
// The faults are injected through voter.FaultHook, a package variable that
// every server of the process shares, so this test does not run in parallel
// with the other tests of the package.
func TestStorageCrashInjection(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			graft, tokens := workerGraft(1)
			cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, unbatched(graft))
			a := cluster.addNode(t)
			b := cluster.addNode(t)

			rel := a.createRepository(t).GetRelativePath()
			onA := a.agent(t, rel, tokens[workerName(0)])
			onB := b.agent(t, rel, tokens[workerName(0)])

			base, err := commitFile(a.ctx, onA, workerName(0), "main", "", "README.md", "base\n")
			require.NoError(t, err)

			t.Run("after upload, before the database commit", func(t *testing.T) {
				headBefore := a.record(t, rel).HeadSeq
				packsBefore := bucketPacks(t, cluster.bucketURL)

				fired := injectFault(t, "after_upload")
				_, err := commitFile(a.ctx, onA, workerName(0), "crash/upload", base, "upload.txt", "lost at the upload\n")
				require.Error(t, err, "the process died before the commit point")
				require.Equal(t, int32(1), fired.Load())

				// Nothing is committed anywhere.
				require.Equal(t, headBefore, a.record(t, rel).HeadSeq, "no changeset was written")
				require.NotContains(t, a.dbRefs(t, rel), "refs/heads/crash/upload")
				require.NotContains(t, a.localRefs(t, rel), "refs/heads/crash/upload")
				_, err = onB.ResolveRevision(b.ctx, "crash/upload")
				require.Error(t, err, "no node serves the lost write")

				// Only an orphan pack is left: uploaded, and referenced by no changeset.
				var orphans []string
				referenced := a.referencedPacks(t, rel)
				for key := range bucketPacks(t, cluster.bucketURL) {
					if !packsBefore[key] {
						require.False(t, referenced[key], "the new pack %s belongs to no changeset", key)
						orphans = append(orphans, key)
					}
				}
				require.Len(t, orphans, 1, "the upload of the dead write is an orphan pack")

				// The client retries; the write lands normally.
				retried, err := commitFile(a.ctx, onA, workerName(0), "crash/upload", base, "upload.txt", "lost at the upload\n")
				require.NoError(t, err)
				got, err := onB.ResolveRevision(b.ctx, "crash/upload")
				require.NoError(t, err)
				require.Equal(t, retried, got)

				a.requireConsistent(t, rel)
				b.requireConsistent(t, rel)
			})

			t.Run("after the database commit, before the git commit", func(t *testing.T) {
				headBefore := a.record(t, rel).HeadSeq

				fired := injectFault(t, "after_commit")
				_, err := commitFile(a.ctx, onA, workerName(0), "crash/commit", base, "commit.txt", "committed in the database only\n")
				require.Error(t, err, "the process died after the commit point")
				require.Equal(t, int32(1), fired.Load())

				// The database is the truth: it has the write.
				rec := a.record(t, rel)
				require.Equal(t, headBefore+1, rec.HeadSeq)
				committed := a.dbRefs(t, rel)["refs/heads/crash/commit"]
				require.False(t, committed.Absent(), "the database has the write")

				// Node A is behind: git never committed the transaction locally, and its
				// watermark has not reached the head. GetStorageState does not catch up.
				require.NotContains(t, a.localRefs(t, rel), "refs/heads/crash/commit")
				state, err := a.operator(t, rel).StorageState(a.ctx)
				require.NoError(t, err)
				require.Equal(t, rec.HeadSeq, state.HeadSequence)
				require.Less(t, state.AppliedSequence, state.HeadSequence, "node-a is behind the database")

				// Another node serves the write: its pack was uploaded before the commit.
				got, err := onB.ResolveRevision(b.ctx, "crash/commit")
				require.NoError(t, err)
				require.Equal(t, string(committed), got)
				content, err := onB.ReadFile(b.ctx, "crash/commit", "commit.txt")
				require.NoError(t, err)
				require.Equal(t, "committed in the database only\n", string(content))

				// The next request on node A catches its copy up.
				got, err = onA.ResolveRevision(a.ctx, "crash/commit")
				require.NoError(t, err)
				require.Equal(t, string(committed), got)
				require.Equal(t, committed, a.localRefs(t, rel)["refs/heads/crash/commit"], "node-a repaired its local copy")

				a.requireConsistent(t, rel)
				b.requireConsistent(t, rel)
			})
		})
	}
}
