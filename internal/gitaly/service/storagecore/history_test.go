package storagecore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/gitops"
	"github.com/feder-cr/open_scion/internal/graft/corestore/sqlstore"
	reviewstore "github.com/feder-cr/open_scion/internal/graft/review/store"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// TestImportRepositoryImportsShadowHistory checks part B of the LOCK AND
// HISTORY RULE: ImportRepository on a tracked repository brings the history
// written in shadow mode since the import (file changelog entries with their
// sequences, file reviews the database does not have) into the database,
// without touching the references, and is idempotent.
func TestImportRepositoryImportsShadowHistory(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeShadow, dialect)
		main := h.seed()
		repoID := h.importRepo()

		before, err := h.core.Meta.GetRepository(h.ctx, corestore.KeyOf(h.repo))
		require.NoError(t, err)
		require.EqualValues(t, 2, before.LogSeq)
		dbRefsBefore, _, err := h.core.Meta.Refs(h.ctx, repoID)
		require.NoError(t, err)

		// Shadow mode writes the file changelog and file reviews only.
		_, err = changelog.Append(h.repoPath, changelog.Meta{Operation: "land", Actor: "carol"}, []changelog.Update{
			{Reference: "refs/heads/feature", OldOID: zeroOID(), NewOID: main.String()},
			{Reference: "refs/heads/other", OldOID: zeroOID(), NewOID: main.String()},
		})
		require.NoError(t, err)
		shadowReview, err := reviewstore.Open(h.repoPath).Create(&gitalypb.Review{
			TargetBranch: []byte("main"),
			HeadCommitId: main.String(),
			Title:        []byte("written in shadow mode"),
		})
		require.NoError(t, err)

		res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Equal(t, before.HeadSeq, res.GetSequence(), "the current head")
		require.Zero(t, res.GetReferences(), "references are not touched")
		require.Zero(t, res.GetPackBytes())
		require.EqualValues(t, 2, res.GetChangelogEntries())
		require.EqualValues(t, 1, res.GetReviews())

		entries, err := h.core.Meta.ScanChangelog(h.ctx, repoID, 0, 0)
		require.NoError(t, err)
		require.Len(t, entries, 4)
		for i, entry := range entries {
			require.EqualValues(t, i+1, entry.Sequence, "the sequences of the file changelog are kept")
		}
		require.Equal(t, "refs/heads/other", entries[3].Reference)
		require.Equal(t, "carol", entries[3].Actor)

		review, _, err := h.core.Meta.GetReview(h.ctx, repoID, shadowReview.GetId())
		require.NoError(t, err)
		require.Equal(t, "written in shadow mode", string(review.GetTitle()))

		after, err := h.core.Meta.GetRepository(h.ctx, corestore.KeyOf(h.repo))
		require.NoError(t, err)
		require.EqualValues(t, 4, after.LogSeq)
		require.Equal(t, before.HeadSeq, after.HeadSeq)
		dbRefsAfter, _, err := h.core.Meta.Refs(h.ctx, repoID)
		require.NoError(t, err)
		require.Equal(t, dbRefsBefore, dbRefsAfter)

		// Idempotent.
		res, err = h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Zero(t, res.GetChangelogEntries())
		require.Zero(t, res.GetReviews())
		entries, err = h.core.Meta.ScanChangelog(h.ctx, repoID, 0, 0)
		require.NoError(t, err)
		require.Len(t, entries, 4)
	})
}

// TestImportRepositoryImportsShadowReviewChanges checks that a review that the
// first import copied into the database and that was then changed in shadow
// mode (in the file review store only) reaches the database with the next
// import, and that the import stays idempotent.
func TestImportRepositoryImportsShadowReviewChanges(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeShadow, dialect)
		h.seed()
		repoID := h.importRepo()

		reviews, err := h.core.Meta.ListReviews(h.ctx, repoID, "", false, 0)
		require.NoError(t, err)
		require.Len(t, reviews, 1)
		openID := reviews[0].GetId()

		// The update time of the file review must be later than the imported one.
		time.Sleep(time.Millisecond)
		_, err = reviewstore.Open(h.repoPath).Update(openID, func(r *gitalypb.Review) error {
			r.State = gitalypb.Review_STATE_CLOSED
			return nil
		})
		require.NoError(t, err)

		res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Zero(t, res.GetChangelogEntries())
		require.EqualValues(t, 1, res.GetReviews(), "the review changed in shadow mode")

		review, version, err := h.core.Meta.GetReview(h.ctx, repoID, openID)
		require.NoError(t, err)
		require.Equal(t, gitalypb.Review_STATE_CLOSED, review.GetState())
		require.EqualValues(t, 2, version)

		res, err = h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Zero(t, res.GetReviews())
	})
}

// TestImportRepositoryHistoryNeedsTheImportedCopy checks that the history is
// only taken from the local copy that was imported into the registration: a
// local repository without the registration's watermark at the same path is
// AlreadyExists, and nothing is written.
func TestImportRepositoryHistoryNeedsTheImportedCopy(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeShadow, dialect)
		h.seed()
		repoID := h.importRepo()

		require.NoError(t, gitops.WriteWatermark(h.repoPath, corestore.Watermark{RepoID: repoID + 100, Seq: 1}))
		_, err := changelog.Append(h.repoPath, changelog.Meta{Operation: "land"}, []changelog.Update{
			{Reference: "refs/heads/feature", OldOID: zeroOID(), NewOID: zeroOID()},
		})
		require.NoError(t, err)

		_, err = h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		testhelper.RequireGrpcCode(t, err, codes.AlreadyExists)

		entries, err := h.core.Meta.ScanChangelog(h.ctx, repoID, 0, 0)
		require.NoError(t, err)
		require.Len(t, entries, 2, "nothing is imported")
	})
}
