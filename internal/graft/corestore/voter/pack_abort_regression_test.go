package voter_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/gitops"
	"github.com/feder-cr/open_scion/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// bucketRepository indexes the pack of every changeset of the fixture's
// repository from the bucket into a new empty repository and returns it: what
// a node that hydrates the repository has.
func (f *fixture) bucketRepository(t *testing.T) *localrepo.Repo {
	t.Helper()

	r, err := f.core.Meta.GetRepository(f.ctx, corestore.KeyOf(f.repo))
	require.NoError(t, err)
	changesets, err := f.core.Meta.ListChangesets(f.ctx, f.repoID, 0, r.HeadSeq)
	require.NoError(t, err)

	emptyProto, _ := gittest.CreateRepository(t, f.ctx, f.cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	empty := localrepo.NewTestRepo(t, f.cfg, emptyProto)

	for _, cs := range changesets {
		if cs.PackKey == "" {
			continue
		}
		blob, err := f.core.Blobs.Get(f.ctx, cs.PackKey)
		require.NoError(t, err)
		require.NoError(t, gitops.IndexPack(f.ctx, empty, blob))
		require.NoError(t, blob.Close())
	}

	return empty
}

// requireCommits checks that repo has every commit of oids.
func requireCommits(t *testing.T, f *fixture, repo *localrepo.Repo, oids ...git.ObjectID) {
	t.Helper()

	for _, oid := range oids {
		ok, err := repo.HasRevision(f.ctx, git.Revision(oid.String()+"^{commit}"))
		require.NoError(t, err)
		require.True(t, ok, "the bucket holds commit %s", oid)
	}
}

// TestShadowPackAfterUnrecordedWrite is the regression test of a shadow-mode
// write that the storage core does not record (here: while the import is not
// complete) and that a later forced update makes invisible: the database then
// equals git, so VerifyRepository is clean, and the bucket must hold the whole
// history of the recorded value. A pack built against the local references
// (`--not --all`) left out the objects of the unrecorded write, so a node that
// hydrated the repository after the switch to authoritative mode missed them.
func TestShadowPackAfterUnrecordedWrite(t *testing.T) {
	const ref = "refs/heads/x"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, withPreparing := range []bool{true, false} {
			name := "prepared only"
			if withPreparing {
				name = "preparing and prepared"
			}

			t.Run(name, func(t *testing.T) {
				f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect, incomplete: true})

				write := func(input []byte, oid git.ObjectID) {
					if withPreparing {
						f.preparing(input)
					}
					require.NoError(t, f.prepared(input))
					gittest.WriteRef(t, f.cfg, f.repoPath, ref, oid)
					f.voter.Committed(f.ctx, f.payload, input)
				}

				// The write runs while the import is not complete: it is not recorded.
				write(line(f.zero, f.a.String(), ref), f.a)
				require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_untracked_writes_total", nil))

				// The import completes with the references it read before the write.
				_, err := f.core.Meta.CommitChangeset(f.ctx, corestore.ChangesetInput{
					RepoID:    f.repoID,
					Updates:   []corestore.RefUpdate{{Name: "HEAD", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")}},
					Operation: "import",
					Actor:     "test",
					NodeID:    "node-a",
				})
				require.NoError(t, err)

				// A forced update (zero old value, the local reference exists):
				// ExpectAny, no conflict, no divergence.
				write(line(f.zero, f.c.String(), ref), f.c)

				require.Zero(t, metricValue(t, f.voter, "graft_storage_shadow_divergences_total", nil))
				require.Zero(t, metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))

				local, err := gitops.ReadRefs(f.ctx, localrepo.NewTestRepo(t, f.cfg, f.repo))
				require.NoError(t, err)
				db, _, err := f.core.Meta.Refs(f.ctx, f.repoID)
				require.NoError(t, err)
				require.Equal(t, local, db, "the database follows git")

				requireCommits(t, f, f.bucketRepository(t), f.a, f.b, f.c)
			})
		}
	})
}

// TestShadowPackAfterWriteWithoutHook covers a local reference that changed
// without the hook (an RPC that bypasses it): the shadow pack of the next
// write must not leave out what that reference reaches.
func TestShadowPackAfterWriteWithoutHook(t *testing.T) {
	const ref = "refs/heads/x"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect})
		gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.b)

		input := line(f.zero, f.c.String(), ref)
		require.NoError(t, f.prepared(input))
		require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref))

		requireCommits(t, f, f.bucketRepository(t), f.a, f.b, f.c)
	})
}

// TestShadowPackExcludesRecordedValues checks that the shadow pack still leaves
// out what the values the database records reach (they are in earlier packs),
// and that a recorded value whose object the repository does not have does not
// fail the write.
func TestShadowPackExcludesRecordedValues(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect})
		f.seed(t, "refs/heads/main", corestore.Target(f.a.String()))
		f.seed(t, "refs/heads/gone", corestore.Target(strings.Repeat("1", len(f.zero))))

		require.NoError(t, f.prepared(line(f.zero, f.c.String(), "refs/heads/next")))
		require.Zero(t, metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))

		changeset := f.lastChangeset(t)
		require.NotEmpty(t, changeset.PackKey)
		blob, err := f.core.Blobs.Get(f.ctx, changeset.PackKey)
		require.NoError(t, err)
		defer func() { require.NoError(t, blob.Close()) }()

		emptyProto, _ := gittest.CreateRepository(t, f.ctx, f.cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
		empty := localrepo.NewTestRepo(t, f.cfg, emptyProto)
		require.NoError(t, gitops.IndexPack(f.ctx, empty, blob))

		requireCommits(t, f, empty, f.b, f.c)
		ok, err := empty.HasRevision(f.ctx, git.Revision(f.a.String()+"^{commit}"))
		require.NoError(t, err)
		require.False(t, ok, "the pack does not hold what a recorded value reaches")

		// A transaction that only points a reference at a recorded value has no pack.
		require.NoError(t, f.prepared(line(f.zero, f.a.String(), "refs/heads/again")))
		require.Empty(t, f.lastChangeset(t).PackKey)
	})
}

// TestAbortedOfTwinTransaction is the regression test of two concurrent
// transactions with the same hook input (two creations of the same branch at
// the same commit): the first commits in the database and in git, the second
// waits for the reference lock and then fails in git without reaching Prepared.
// Its Aborted phase must not take the pending entry of the first: no local
// commit failure is counted, and the Committed phase of the first still notes
// its sequence.
func TestAbortedOfTwinTransaction(t *testing.T) {
	const ref = "refs/heads/twin"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		t.Run("creation", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})

			input := line(f.zero, f.a.String(), ref)
			f.preparing(input)                               // T1
			f.preparing(input)                               // T2
			require.NoError(t, f.prepared(input))            // T1 commits in the database ...
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a) // ... and in git.

			f.voter.Aborted(f.ctx, f.payload, input) // T2
			require.Zero(t, metricValue(t, f.voter, "graft_storage_local_commit_failures_total", nil))

			f.voter.Committed(f.ctx, f.payload, input) // T1
			require.Equal(t, []noteCall{{repoPath: f.repoPath, repoID: f.repoID, seq: f.seq(1)}}, f.applier.calls)
		})

		t.Run("deletion", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
			f.seed(t, ref, corestore.Target(f.a.String()))

			input := line(f.a.String(), f.zero, ref)
			require.NoError(t, f.prepared(input))
			gittest.Exec(t, f.cfg, "-C", f.repoPath, "update-ref", "-d", ref)

			f.voter.Aborted(f.ctx, f.payload, input)
			require.Zero(t, metricValue(t, f.voter, "graft_storage_local_commit_failures_total", nil))

			f.voter.Committed(f.ctx, f.payload, input)
			require.Equal(t, []noteCall{{repoPath: f.repoPath, repoID: f.repoID, seq: f.seq(2)}}, f.applier.calls)
		})

		t.Run("git failed to commit the committed transaction", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})

			// The local repository does not have the new value: a real failure.
			input := line(f.zero, f.a.String(), ref)
			require.NoError(t, f.prepared(input))
			f.voter.Aborted(f.ctx, f.payload, input)
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_local_commit_failures_total", nil))

			f.voter.Committed(f.ctx, f.payload, input)
			require.Empty(t, f.applier.calls)
		})

		t.Run("same input in another storage", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})

			input := line(f.zero, f.a.String(), ref)
			require.NoError(t, f.prepared(input))

			other := f.payload
			other.Repo = &gitalypb.Repository{StorageName: "other", RelativePath: f.repo.GetRelativePath()}
			f.voter.Aborted(f.ctx, other, input)
			require.Zero(t, metricValue(t, f.voter, "graft_storage_local_commit_failures_total", nil),
				"the entry of another storage is not taken")

			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
			f.voter.Committed(f.ctx, f.payload, input)
			require.Equal(t, []noteCall{{repoPath: f.repoPath, repoID: f.repoID, seq: f.seq(1)}}, f.applier.calls)
		})
	})
}
