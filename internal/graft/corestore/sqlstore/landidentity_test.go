package sqlstore_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// landMeta is the changelog metadata of a land of original on reference as landed.
func landMeta(reference string, lands ...changelog.Land) *changelog.Meta {
	return &changelog.Meta{Operation: "land", Actor: "agent", LandReference: reference, Lands: lands}
}

// requireLand checks the land record of original on reference.
func requireLand(t *testing.T, ctx context.Context, s *sqlstore.Store, repoID int64, reference string, original corestore.Target, want changelog.LandRecord) {
	t.Helper()

	record, found, err := s.LookupLand(ctx, repoID, reference, string(original))
	require.NoError(t, err)
	require.True(t, found, "land of %s on %s", original, reference)
	require.Equal(t, want, record)
}

// requireNoLand checks that no land of original on reference is recorded.
func requireNoLand(t *testing.T, ctx context.Context, s *sqlstore.Store, repoID int64, reference string, original corestore.Target) {
	t.Helper()

	_, found, err := s.LookupLand(ctx, repoID, reference, string(original))
	require.NoError(t, err)
	require.False(t, found, "land of %s on %s", original, reference)
}

// TestLandIdentities verifies the land identities of the database (doc/graft/PLAN11.md, 4.1): the
// changeset of a land records its lands with its changelog entry, a later land of the same commit
// replaces the record, and the identity is per repository and branch.
func TestLandIdentities(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("lands"))
		other := register(t, ctx, s, testKey("lands-other"))

		requireNoLand(t, ctx, s, repo.ID, "refs/heads/main", oid(10))

		// A batch of two lands on main, with an update of another branch in the same changeset,
		// which records no land.
		result, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID: repo.ID,
			Updates: []corestore.RefUpdate{
				create("refs/heads/side", oid(9)),
				create("refs/heads/main", oid(12)),
			},
			Changelog: landMeta("refs/heads/main",
				changelog.Land{Original: string(oid(10)), Landed: string(oid(11))},
				changelog.Land{Original: string(oid(20)), Landed: string(oid(12))},
			),
		})
		require.NoError(t, err)
		require.Len(t, result.Changelog, 2)
		require.Empty(t, result.Changelog[0].Lands)
		require.Equal(t, []changelog.Land{
			{Original: string(oid(10)), Landed: string(oid(11))},
			{Original: string(oid(20)), Landed: string(oid(12))},
		}, result.Changelog[1].Lands)
		batchSeq := result.Changelog[1].Sequence

		entries, err := s.ScanChangelog(ctx, repo.ID, 0, 0)
		require.NoError(t, err)
		require.Equal(t, result.Changelog, entries, "the entries keep their lands")

		requireLand(t, ctx, s, repo.ID, "refs/heads/main", oid(10), changelog.LandRecord{Sequence: batchSeq, Landed: string(oid(11))})
		requireLand(t, ctx, s, repo.ID, "refs/heads/main", oid(20), changelog.LandRecord{Sequence: batchSeq, Landed: string(oid(12))})
		requireNoLand(t, ctx, s, repo.ID, "refs/heads/side", oid(10))
		requireNoLand(t, ctx, s, other.ID, "refs/heads/main", oid(10))

		// A later land of the same commit replaces its record.
		result, err = s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID:    repo.ID,
			Updates:   []corestore.RefUpdate{update("refs/heads/main", oid(12), oid(13))},
			Changelog: landMeta("refs/heads/main", changelog.Land{Original: string(oid(10)), Landed: string(oid(13))}),
		})
		require.NoError(t, err)
		requireLand(t, ctx, s, repo.ID, "refs/heads/main", oid(10), changelog.LandRecord{Sequence: result.Changelog[0].Sequence, Landed: string(oid(13))})
		requireLand(t, ctx, s, repo.ID, "refs/heads/main", oid(20), changelog.LandRecord{Sequence: batchSeq, Landed: string(oid(12))})

		// A branch whose name is not valid UTF-8.
		result, err = s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID:    repo.ID,
			Updates:   []corestore.RefUpdate{create("refs/heads/\xff", oid(31))},
			Changelog: landMeta("refs/heads/\xff", changelog.Land{Original: string(oid(30)), Landed: string(oid(31))}),
		})
		require.NoError(t, err)
		requireLand(t, ctx, s, repo.ID, "refs/heads/\xff", oid(30), changelog.LandRecord{Sequence: result.Changelog[0].Sequence, Landed: string(oid(31))})
		requireNoLand(t, ctx, s, repo.ID, "refs/heads/�", oid(30))
	})
}

// TestLandIdentitiesAreWrittenWithTheChangeset verifies that the land record and the changeset are
// one transaction: when the record cannot be written, the changeset is not committed either.
func TestLandIdentitiesAreWrittenWithTheChangeset(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("lands-atomic"))
		_, err := commit(ctx, s, repo.ID, create("refs/heads/main", oid(1)))
		require.NoError(t, err)

		execSQL(t, ctx, s, "DROP TABLE graft_land_identities")

		_, err = s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID:    repo.ID,
			Updates:   []corestore.RefUpdate{update("refs/heads/main", oid(1), oid(3))},
			Changelog: landMeta("refs/heads/main", changelog.Land{Original: string(oid(2)), Landed: string(oid(3))}),
		})
		require.ErrorContains(t, err, "insert land identity of changelog entry 1")

		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{"refs/heads/main": oid(1)})
		entries, err := s.ScanChangelog(ctx, repo.ID, 0, 0)
		require.NoError(t, err)
		require.Empty(t, entries)
		after, err := s.GetRepository(ctx, testKey("lands-atomic"))
		require.NoError(t, err)
		require.Equal(t, repo.HeadSeq+1, after.HeadSeq, "only the changeset without a land was committed")
	})
}

// TestLandIdentitiesOfImportedHistory verifies that history written outside the database keeps
// its land identities when it is brought in: the initial history of a registered repository and
// the history that shadow mode imports.
func TestLandIdentitiesOfImportedHistory(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		landed := func(seq uint64, original, landed corestore.Target) changelog.Entry {
			return changelog.Entry{
				Sequence:  seq,
				Reference: "refs/heads/main",
				OldOID:    string(oid(0)),
				NewOID:    string(landed),
				Operation: "land",
				Lands:     []changelog.Land{{Original: string(original), Landed: string(landed)}},
			}
		}

		repo, _, err := s.RegisterRepository(ctx, testKey("lands-import"), "sha1", corestore.InitialState{
			Changelog: []changelog.Entry{landed(1, oid(10), oid(11)), landed(2, oid(20), oid(21))},
		})
		require.NoError(t, err)
		requireLand(t, ctx, s, repo.ID, "refs/heads/main", oid(10), changelog.LandRecord{Sequence: 1, Landed: string(oid(11))})
		requireLand(t, ctx, s, repo.ID, "refs/heads/main", oid(20), changelog.LandRecord{Sequence: 2, Landed: string(oid(21))})

		imported, _, err := s.ImportHistory(ctx, repo.ID, []changelog.Entry{landed(3, oid(10), oid(31)), landed(4, oid(40), oid(41))}, nil)
		require.NoError(t, err)
		require.Equal(t, 2, imported)
		requireLand(t, ctx, s, repo.ID, "refs/heads/main", oid(10), changelog.LandRecord{Sequence: 3, Landed: string(oid(31))})
		requireLand(t, ctx, s, repo.ID, "refs/heads/main", oid(40), changelog.LandRecord{Sequence: 4, Landed: string(oid(41))})
	})
}

// TestLandIdentitiesGoWithTheRepository verifies that the land identities of a removed repository
// are removed with it, and that a repository registered again under its key has none.
func TestLandIdentitiesGoWithTheRepository(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		key := testKey("lands-removed")
		first := register(t, ctx, s, key)
		_, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID:    first.ID,
			Updates:   []corestore.RefUpdate{create("refs/heads/main", oid(11))},
			Changelog: landMeta("refs/heads/main", changelog.Land{Original: string(oid(10)), Landed: string(oid(11))}),
		})
		require.NoError(t, err)

		deleted, err := s.DeleteRepository(ctx, key)
		require.NoError(t, err)
		require.True(t, deleted)
		requireNoLand(t, ctx, s, first.ID, "refs/heads/main", oid(10))

		second := register(t, ctx, s, key)
		requireNoLand(t, ctx, s, second.ID, "refs/heads/main", oid(10))

		var rows int
		require.NoError(t, s.DB().QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM graft_land_identities WHERE repo_id = %d", first.ID)).Scan(&rows))
		require.Zero(t, rows, "the rows of the removed repository are gone")
	})
}
