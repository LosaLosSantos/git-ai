package sqlstore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// keepAround commits the keep-around reference of root.
func keepAround(t *testing.T, ctx context.Context, s *sqlstore.Store, repoID int64, root corestore.Target) {
	t.Helper()
	_, err := commit(ctx, s, repoID, create(corestore.KeepAroundRef(string(root)), root))
	require.NoError(t, err)
}

// TestReviewWritesRequireRootReferences: a review write that requires its
// roots is refused, writing nothing, while its head or base commit has no
// keep-around reference with that value (doc/graft/PLAN7.md, REVIEW ROOTS
// RULE).
func TestReviewWritesRequireRootReferences(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo, _ := headRepo(t, ctx, s, "root-refs", 1)
		head, base := oid(10), oid(11)
		build := func(head, base corestore.Target) func(string) (*gitalypb.Review, error) {
			return func(string) (*gitalypb.Review, error) {
				return &gitalypb.Review{TargetBranch: []byte("main"), HeadCommitId: string(head), BaseCommitId: string(base)}, nil
			}
		}
		requireReviews := func(n int) {
			t.Helper()
			reviews, err := s.ListReviews(ctx, repo.ID, "", true, 0)
			require.NoError(t, err)
			require.Len(t, reviews, n)
		}

		_, err := s.InsertReview(ctx, repo.ID, build(head, base), corestore.ReviewInsert{RequireRoots: true})
		require.ErrorIs(t, err, corestore.ErrReviewRootsMissing, "no reference at all")
		keepAround(t, ctx, s, repo.ID, head)
		_, err = s.InsertReview(ctx, repo.ID, build(head, base), corestore.ReviewInsert{RequireRoots: true})
		require.ErrorIs(t, err, corestore.ErrReviewRootsMissing, "the base has none")
		requireReviews(0)

		// A keep-around reference with another value does not protect the commit.
		_, err = commit(ctx, s, repo.ID, create(corestore.KeepAroundRef(string(base)), oid(12)))
		require.NoError(t, err)
		_, err = s.InsertReview(ctx, repo.ID, build(head, base), corestore.ReviewInsert{RequireRoots: true})
		require.ErrorIs(t, err, corestore.ErrReviewRootsMissing)
		_, err = commit(ctx, s, repo.ID, update(corestore.KeepAroundRef(string(base)), oid(12), base))
		require.NoError(t, err)

		created, err := s.InsertReview(ctx, repo.ID, build(head, base), corestore.ReviewInsert{RequireRoots: true})
		require.NoError(t, err)
		requireReviews(1)

		// A head move to an unprotected commit is refused and writes nothing.
		r, version, err := s.GetReview(ctx, repo.ID, created.GetId())
		require.NoError(t, err)
		r.HeadCommitId = string(oid(13))
		require.ErrorIs(t, s.UpdateReview(ctx, repo.ID, created.GetId(), version, r, true), corestore.ErrReviewRootsMissing)
		unchanged, unchangedVersion, err := s.GetReview(ctx, repo.ID, created.GetId())
		require.NoError(t, err)
		require.Equal(t, version, unchangedVersion)
		require.Equal(t, string(head), unchanged.GetHeadCommitId())

		keepAround(t, ctx, s, repo.ID, oid(13))
		require.NoError(t, s.UpdateReview(ctx, repo.ID, created.GetId(), version, r, true))

		// Writes that do not require the roots are not conditioned: imports and
		// comments.
		_, err = s.InsertReview(ctx, repo.ID, build(oid(14), ""), corestore.ReviewInsert{})
		require.NoError(t, err)
		r.Title = []byte("a comment")
		require.NoError(t, s.UpdateReview(ctx, repo.ID, created.GetId(), version+1, r, false))

		// A required write of a removed repository finds no repository row.
		gone, _ := headRepo(t, ctx, s, "root-refs-gone", 1)
		_, err = s.InsertReview(ctx, gone.ID, build(oid(15), ""), corestore.ReviewInsert{})
		require.NoError(t, err)
		removed, err := s.DeleteRepository(ctx, gone.Key)
		require.NoError(t, err)
		require.True(t, removed)
		require.ErrorIs(t, s.UpdateReview(ctx, gone.ID, "1", 1, r, true), corestore.ErrRepositoryNotFound)
	})
}

// TestOpenReviewRootsCannotBeReleased: a changeset that deletes or moves the
// keep-around reference of an open review's head or base is refused, writing
// nothing; forced changesets (shadow mode) are not, and closed reviews keep
// nothing.
func TestOpenReviewRootsCannotBeReleased(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo, _ := headRepo(t, ctx, s, "release-roots", 1)
		head, base, closed := oid(10), oid(11), oid(12)
		for _, c := range []corestore.Target{head, base, closed} {
			keepAround(t, ctx, s, repo.ID, c)
		}
		open, err := s.InsertReview(ctx, repo.ID, func(string) (*gitalypb.Review, error) {
			return &gitalypb.Review{TargetBranch: []byte("main"), HeadCommitId: string(head), BaseCommitId: string(base)}, nil
		}, corestore.ReviewInsert{RequireRoots: true})
		require.NoError(t, err)
		_, err = s.InsertReview(ctx, repo.ID, func(string) (*gitalypb.Review, error) {
			return &gitalypb.Review{TargetBranch: []byte("main"), HeadCommitId: string(closed), State: gitalypb.Review_STATE_CLOSED}, nil
		}, corestore.ReviewInsert{RequireRoots: true})
		require.NoError(t, err)

		before, seq, err := s.Refs(ctx, repo.ID)
		require.NoError(t, err)
		for desc, updates := range map[string][]corestore.RefUpdate{
			"delete the head's":          {update(corestore.KeepAroundRef(string(head)), head, "")},
			"delete the base's":          {update(corestore.KeepAroundRef(string(base)), base, "")},
			"move the head's":            {update(corestore.KeepAroundRef(string(head)), head, oid(99))},
			"force-delete the head's":    {force(corestore.KeepAroundRef(string(head)), "")},
			"with an unrelated deletion": {update(corestore.KeepAroundRef(string(closed)), closed, ""), update(corestore.KeepAroundRef(string(base)), base, "")},
		} {
			_, err := commit(ctx, s, repo.ID, updates...)
			require.ErrorIs(t, err, corestore.ErrReviewRootInUse, desc)
			after, afterSeq, err := s.Refs(ctx, repo.ID)
			require.NoError(t, err)
			require.Equal(t, before, after, "%s: nothing written", desc)
			require.Equal(t, seq, afterSeq)
		}

		// A closed review's commit is not a root.
		_, err = commit(ctx, s, repo.ID, update(corestore.KeepAroundRef(string(closed)), closed, ""))
		require.NoError(t, err)

		// GitLab's own keep-around reference of the same commit is not a review
		// root: it is not guarded.
		gitlab := "refs/keep-around/" + string(head)
		_, err = commit(ctx, s, repo.ID, create(gitlab, head))
		require.NoError(t, err)
		_, err = commit(ctx, s, repo.ID, update(gitlab, head, ""))
		require.NoError(t, err)

		// Shadow mode records what git did: forced changesets are not refused.
		_, err = s.CommitChangeset(ctx, corestore.ChangesetInput{
			RepoID:    repo.ID,
			Updates:   []corestore.RefUpdate{update(corestore.KeepAroundRef(string(base)), base, "")},
			Operation: "test",
			Actor:     "tester",
			NodeID:    "node-a",
			Force:     true,
		})
		require.NoError(t, err)

		// Once the review is closed its commits are released.
		r, version, err := s.GetReview(ctx, repo.ID, open.GetId())
		require.NoError(t, err)
		r.State = gitalypb.Review_STATE_CLOSED
		require.NoError(t, s.UpdateReview(ctx, repo.ID, open.GetId(), version, r, false))
		_, err = commit(ctx, s, repo.ID, update(corestore.KeepAroundRef(string(head)), head, ""))
		require.NoError(t, err)
	})
}
