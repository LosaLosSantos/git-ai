package voter_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// admissionHeld reports whether a writer holds the admission of the fixture's
// repository: an Acquire with a cancelled context is admitted at once when
// nobody holds it, and gives up otherwise.
func (f *fixture) admissionHeld(t *testing.T) bool {
	t.Helper()

	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	probe := localrefs.Key{0xaa, 0xbb}
	if f.core.RefAdmission.Acquire(ctx, f.repoPath, probe, localrefs.WriterSyncer) {
		f.core.RefAdmission.Release(f.repoPath, probe)
		return false
	}
	return true
}

// markers returns the dirty markers of the fixture's storage.
func (f *fixture) markers(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(f.cfg.Storages[0].Path, localrefs.MarkerDirName))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// TestLocalReferenceWritesOfACommittedTransaction follows one authoritative
// transaction through the LOCAL REFERENCE WRITES RULE (doc/graft/PLAN7.md):
// admitted at the end of Preparing, marked dirty and in flight after the
// database commit in Prepared, released and ended in Committed.
func TestLocalReferenceWritesOfACommittedTransaction(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	const ref = "refs/heads/main"
	input := line(f.zero, f.a.String(), ref)

	require.True(t, f.voter.DeferredReferenceDurability())
	require.False(t, f.admissionHeld(t))

	f.voter.Preparing(f.ctx, f.payload, input)
	require.True(t, f.admissionHeld(t), "the transaction is admitted at the end of Preparing")
	require.Empty(t, f.markers(t), "nothing is dirty before the database commit")

	require.NoError(t, f.prepared(input))
	require.Len(t, f.markers(t), 1)

	// git may still write and compact until Committed: the durable step keeps
	// the marker.
	require.NoError(t, f.core.RefDurability.Settle(f.repoPath, false))
	require.Len(t, f.markers(t), 1)

	gittest.WriteRef(t, f.cfg, f.repoPath, git.ReferenceName(ref), f.a)
	f.voter.Committed(f.ctx, f.payload, input)
	require.False(t, f.admissionHeld(t), "Committed releases the admission")
	require.Len(t, f.applier.calls, 1, "the local commit is still noted")

	require.NoError(t, f.core.RefDurability.Settle(f.repoPath, false))
	require.Empty(t, f.markers(t))
}

// TestLocalReferenceWritesOfAnAbortedTransaction verifies that an abort after
// Prepared releases the admission and ends the transaction: the repository is
// settled once nothing else is live.
func TestLocalReferenceWritesOfAnAbortedTransaction(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	input := line(f.zero, f.a.String(), "refs/heads/main")

	f.voter.Preparing(f.ctx, f.payload, input)
	require.NoError(t, f.prepared(input))
	f.voter.Aborted(f.ctx, f.payload, input)
	require.False(t, f.admissionHeld(t))

	require.NoError(t, f.core.RefDurability.Settle(f.repoPath, false))
	require.Empty(t, f.markers(t))
}

// TestLocalReferenceWritesOfTransactionsWithTheSameInput verifies that two
// live transactions with the same input are both counted: the one denied by
// the policy and aborted does not end the other one, which may still write.
func TestLocalReferenceWritesOfTransactionsWithTheSameInput(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	input := line(f.zero, f.a.String(), "refs/heads/main")

	f.voter.Preparing(f.ctx, f.payload, input)
	require.NoError(t, f.prepared(input))

	// The same input from an agent the policy denies.
	f.voter.Denied(f.ctx, f.payload, input)
	f.voter.Aborted(f.ctx, f.payload, input)
	require.NoError(t, f.core.RefDurability.Settle(f.repoPath, true))
	require.Len(t, f.markers(t), 1, "the first transaction may still write")

	gittest.WriteRef(t, f.cfg, f.repoPath, git.ReferenceName("refs/heads/main"), f.a)
	f.voter.Committed(f.ctx, f.payload, input)
	require.NoError(t, f.core.RefDurability.Settle(f.repoPath, false))
	require.Empty(t, f.markers(t))
}

// TestLocalReferenceWritesOfARejectedTransaction verifies that a transaction
// the database rejects marks nothing: git writes nothing for it.
func TestLocalReferenceWritesOfARejectedTransaction(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	const ref = "refs/heads/main"
	f.seed(t, ref, corestore.Target(f.b.String()))
	input := line(f.a.String(), f.c.String(), ref)

	f.voter.Preparing(f.ctx, f.payload, input)
	requireMismatch(t, f.prepared(input), ref, f.a.String(), f.b.String())
	f.voter.Aborted(f.ctx, f.payload, input)

	require.False(t, f.admissionHeld(t))
	require.Empty(t, f.markers(t))
}

// TestLocalReferenceWritesWhenTheMarkerCannotBeWritten verifies that git does
// not write references without fsync when the repository cannot be marked
// dirty: the transaction is aborted after the database commit, which catch-up
// repairs like any local failure.
func TestLocalReferenceWritesWhenTheMarkerCannotBeWritten(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	// The marker directory exists (the ownership created it); no file can be
	// created in it.
	markerDir := filepath.Join(f.cfg.Storages[0].Path, localrefs.MarkerDirName)
	require.NoError(t, os.Chmod(markerDir, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(markerDir, 0o700)) })

	input := line(f.zero, f.a.String(), "refs/heads/main")
	err := f.prepared(input)
	require.Equal(t, codes.Unavailable, status.Code(err), "error %v", err)
	require.ErrorContains(t, err, "write the dirty marker")
	require.Equal(t, uint64(1), f.headSeq(t), "the database committed the changeset before the marker")
}

// TestDeferredReferenceDurabilityByMode verifies that references are written
// without fsync in authoritative mode with deferred durability only: off and
// shadow modes, and authoritative mode without the boot identity, keep git's
// fsync of references.
func TestDeferredReferenceDurabilityByMode(t *testing.T) {
	t.Parallel()

	var nilVoter *voter.Voter
	require.False(t, nilVoter.DeferredReferenceDurability())

	off := voter.New(&corestore.Core{Mode: corestore.ModeOff}, testhelper.NewLogger(t), localrepo.Factory{}, nil, nil)
	require.False(t, off.DeferredReferenceDurability())

	shadow := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, localRefs: true})
	require.False(t, shadow.voter.DeferredReferenceDurability())

	authoritative := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative})
	require.False(t, authoritative.voter.DeferredReferenceDurability())

	deferred := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	require.True(t, deferred.voter.DeferredReferenceDurability())
}

// TestLocalReferenceWritesNotInShadowMode verifies that shadow mode, where git
// is the source of truth, neither admits nor marks.
func TestLocalReferenceWritesNotInShadowMode(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, localRefs: true})
	input := line(f.zero, f.a.String(), "refs/heads/main")

	f.voter.Preparing(f.ctx, f.payload, input)
	require.False(t, f.admissionHeld(t))
	require.NoError(t, f.prepared(input))
	require.Empty(t, f.markers(t))
	f.voter.Committed(f.ctx, f.payload, input)
}
