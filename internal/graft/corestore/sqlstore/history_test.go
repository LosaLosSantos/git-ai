package sqlstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// historyEntry returns a changelog entry of a branch with the sequence seq.
func historyEntry(seq uint64) changelog.Entry {
	return changelog.Entry{
		Sequence:   seq,
		Reference:  "refs/heads/main",
		OldOID:     strings.Repeat("0", 40),
		NewOID:     string(oid(int(seq))),
		Operation:  "shadow",
		Actor:      "alice",
		RecordedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func reviewWithID(id, title string) *gitalypb.Review {
	r := newReview(title, "main", gitalypb.Review_STATE_OPEN_UNSPECIFIED)
	r.Id = id
	return r
}

// TestImportHistory checks MetaStore.ImportHistory (LOCK AND HISTORY RULE):
// the shadow-period history is appended with its sequences, reviews that the
// database already has and that did not change (same update time) are skipped,
// the next review ID follows the largest
// one, references are untouched, and it is idempotent.
func TestImportHistory(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo, _, err := s.RegisterRepository(ctx, testKey("history"), "sha1", corestore.InitialState{
			Changeset: &corestore.ChangesetInput{
				Operation: "import",
				NodeID:    "node-a",
				Updates:   []corestore.RefUpdate{create("refs/heads/main", oid(1))},
			},
			Changelog: []changelog.Entry{historyEntry(1), historyEntry(2)},
			Reviews:   []*gitalypb.Review{reviewWithID("1", "imported")},
		})
		require.NoError(t, err)
		require.Equal(t, uint64(2), repo.LogSeq)

		entries, reviews, err := s.ImportHistory(ctx, repo.ID,
			[]changelog.Entry{historyEntry(3), historyEntry(5)},
			[]*gitalypb.Review{reviewWithID("1", "no newer update time"), reviewWithID("2", "shadow"), reviewWithID("7", "shadow too")},
		)
		require.NoError(t, err)
		require.Equal(t, 2, entries)
		require.Equal(t, 2, reviews, "the review the database has is skipped")

		current, err := s.GetRepository(ctx, testKey("history"))
		require.NoError(t, err)
		require.Equal(t, uint64(5), current.LogSeq)
		require.Equal(t, repo.HeadSeq, current.HeadSeq, "no changeset is recorded")
		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{"refs/heads/main": oid(1)})

		log, err := s.ScanChangelog(ctx, repo.ID, 0, 0)
		require.NoError(t, err)
		require.Len(t, log, 4)
		for i, seq := range []uint64{1, 2, 3, 5} {
			require.Equal(t, seq, log[i].Sequence)
		}
		require.Equal(t, historyEntry(5).NewOID, log[3].NewOID)

		kept, _, err := s.GetReview(ctx, repo.ID, "1")
		require.NoError(t, err)
		require.Equal(t, "imported", string(kept.GetTitle()))
		shadow, version, err := s.GetReview(ctx, repo.ID, "7")
		require.NoError(t, err)
		require.Equal(t, int64(1), version)
		require.Equal(t, "shadow too", string(shadow.GetTitle()))

		inserted, err := s.InsertReview(ctx, repo.ID, func(id string) (*gitalypb.Review, error) {
			return newReview("next", "main", gitalypb.Review_STATE_OPEN_UNSPECIFIED), nil
		})
		require.NoError(t, err)
		require.Equal(t, "8", inserted.GetId(), "the next review ID follows the largest imported one")

		// A later database changelog entry continues after the imported history.
		later, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID:    repo.ID,
			Updates:   []corestore.RefUpdate{update("refs/heads/main", oid(1), oid(9))},
			Changelog: &changelog.Meta{Operation: "land", Actor: "bob"},
		})
		require.NoError(t, err)
		require.Equal(t, uint64(6), later.Changelog[0].Sequence)

		// Nothing new: idempotent.
		entries, reviews, err = s.ImportHistory(ctx, repo.ID, nil,
			[]*gitalypb.Review{reviewWithID("2", "shadow"), reviewWithID("7", "shadow too")})
		require.NoError(t, err)
		require.Zero(t, entries)
		require.Zero(t, reviews)
	})
}

// TestImportHistoryReplacesReviewsChangedInShadowMode checks that a review
// that the first import copied into the database and that was then changed in
// the file review store (shadow mode) is replaced by its file version with the
// next version, while a review the database changed later, a different review
// that only shares the ID and an unchanged review are kept.
func TestImportHistoryReplacesReviewsChangedInShadowMode(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		t0 := time.Date(2026, 3, 1, 10, 0, 0, 123456789, time.UTC)
		at := func(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(t0.Add(d)) }
		review := func(id, title string, created, updated time.Duration) *gitalypb.Review {
			r := reviewWithID(id, title)
			r.CreatedAt = at(created)
			r.UpdatedAt = at(updated)
			return r
		}

		repo, _, err := s.RegisterRepository(ctx, testKey("shadow-reviews"), "sha1", corestore.InitialState{
			Changeset: &corestore.ChangesetInput{
				Operation: "import",
				NodeID:    "node-a",
				Updates:   []corestore.RefUpdate{create("refs/heads/main", oid(1))},
			},
			Reviews: []*gitalypb.Review{
				review("1", "imported", 0, 0),
				review("2", "imported", 0, 0),
				review("3", "imported", 0, 0),
				review("4", "imported", 0, 0),
			},
		})
		require.NoError(t, err)

		// Review 2 was changed in the database after the file (authoritative mode).
		dbChanged := review("2", "changed in the database", 0, 2*time.Hour)
		require.NoError(t, s.UpdateReview(ctx, repo.ID, "2", 1, dbChanged))

		closed := review("1", "closed in shadow mode", 0, time.Hour)
		closed.State = gitalypb.Review_STATE_CLOSED

		entries, reviews, err := s.ImportHistory(ctx, repo.ID, nil, []*gitalypb.Review{
			closed,
			review("2", "changed in shadow mode", 0, time.Hour),
			review("3", "another review with the same ID", 5*time.Hour, 6*time.Hour),
			review("4", "imported", 0, 0),
			review("5", "created in shadow mode", 3*time.Hour, 3*time.Hour),
		})
		require.NoError(t, err)
		require.Zero(t, entries)
		require.Equal(t, 2, reviews, "the changed review 1 and the new review 5")

		requireReview := func(t *testing.T, id, title string, version int64) *gitalypb.Review {
			t.Helper()
			r, v, err := s.GetReview(ctx, repo.ID, id)
			require.NoError(t, err)
			require.Equal(t, title, string(r.GetTitle()), "review %s", id)
			require.Equal(t, version, v, "version of review %s", id)
			return r
		}

		replaced := requireReview(t, "1", "closed in shadow mode", 2)
		require.Equal(t, gitalypb.Review_STATE_CLOSED, replaced.GetState())
		require.True(t, proto.Equal(at(time.Hour), replaced.GetUpdatedAt()))
		requireReview(t, "2", "changed in the database", 2)
		requireReview(t, "3", "imported", 1)
		requireReview(t, "4", "imported", 1)
		requireReview(t, "5", "created in shadow mode", 1)

		// The closed column follows the replaced review.
		open, err := s.ListReviews(ctx, repo.ID, "", false, 0)
		require.NoError(t, err)
		var openIDs []string
		for _, r := range open {
			openIDs = append(openIDs, r.GetId())
		}
		require.Equal(t, []string{"5", "4", "3", "2"}, openIDs)

		// An update based on the version before the import re-reads the review.
		require.ErrorIs(t, s.UpdateReview(ctx, repo.ID, "1", 1, review("1", "stale", 0, 4*time.Hour)), corestore.ErrReviewVersionConflict)

		// Idempotent: the same file reviews write nothing new.
		_, reviews, err = s.ImportHistory(ctx, repo.ID, nil, []*gitalypb.Review{closed, review("5", "created in shadow mode", 3*time.Hour, 3*time.Hour)})
		require.NoError(t, err)
		require.Zero(t, reviews)
		requireReview(t, "1", "closed in shadow mode", 2)
	})
}

// TestImportHistoryRejectsReusedSequences checks that entries whose sequences
// the database changelog already used, or that are not strictly increasing,
// are rejected and nothing is written.
func TestImportHistoryRejectsReusedSequences(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo, _, err := s.RegisterRepository(ctx, testKey("reused"), "sha1", corestore.InitialState{
			Changelog: []changelog.Entry{historyEntry(1), historyEntry(2)},
		})
		require.NoError(t, err)

		for name, entries := range map[string][]changelog.Entry{
			"sequence already used":   {historyEntry(2), historyEntry(3)},
			"not strictly increasing": {historyEntry(4), historyEntry(4)},
			"zero sequence":           {{Sequence: 0}},
		} {
			t.Run(name, func(t *testing.T) {
				_, _, err := s.ImportHistory(ctx, repo.ID, entries, []*gitalypb.Review{reviewWithID("1", "not written")})
				require.Error(t, err)

				current, err := s.GetRepository(ctx, testKey("reused"))
				require.NoError(t, err)
				require.Equal(t, uint64(2), current.LogSeq)
				log, err := s.ScanChangelog(ctx, repo.ID, 0, 0)
				require.NoError(t, err)
				require.Len(t, log, 2)
				_, _, err = s.GetReview(ctx, repo.ID, "1")
				require.ErrorIs(t, err, corestore.ErrReviewNotFound, "nothing is written")
			})
		}

		_, _, err = s.ImportHistory(ctx, repo.ID+1000, nil, nil)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
	})
}
