package gate_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	housekeepingcfg "github.com/feder-cr/git-ai/internal/git/housekeeping/config"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// housekeep runs the object housekeeping of Gitaly on the node's local copy
// of repo with an expiry of now: a full repack that drops unreachable objects
// into an expired cruft pack, then a prune of the unreachable loose objects.
// Production housekeeping does the same with a grace period of two weeks.
func housekeep(t *testing.T, n node, repo *gitalypb.Repository) {
	t.Helper()

	ctx := testhelper.Context(t)
	local := localrepo.NewTestRepo(t, n.cfg, repo)
	now := time.Now()
	require.NoError(t, housekeeping.RepackObjects(ctx, local, housekeepingcfg.RepackObjectsConfig{
		Strategy:          housekeepingcfg.RepackObjectsStrategyFullWithCruft,
		CruftExpireBefore: now,
	}))
	require.NoError(t, housekeeping.PruneObjects(ctx, local, housekeeping.PruneObjectsConfig{ExpireBefore: now}))
}

// hasObject reports whether the node's local copy of repo has the object.
func hasObject(t *testing.T, n node, repo *gitalypb.Repository, oid git.ObjectID) bool {
	t.Helper()
	path := n.cfg.Storages[0].Path + "/" + repo.GetRelativePath()
	return gittest.NewCommand(t, n.cfg, "-C", path, "cat-file", "-e", oid.String()).Run() == nil
}

func (n node) deleteBranch(t *testing.T, repo *gitalypb.Repository, name string) {
	t.Helper()
	_, err := gitalypb.NewOperationServiceClient(n.conn).UserDeleteBranch(n.ctx, &gitalypb.UserDeleteBranchRequest{
		Repository: repo,
		BranchName: []byte(name),
		User:       gittest.TestUser,
	})
	require.NoError(t, err)
}

// TestKeepAroundReferencesOfOpenReviewsAreGuarded: in authoritative mode no
// write path can delete or move the keep-around reference of an open review's
// commit: DeleteRefs, WriteRef and UpdateReferences (git update-ref through
// the reference-transaction hook) are refused by the database and change
// nothing, locally or in the database. A keep-around reference that no open
// review uses can be deleted.
func TestKeepAroundReferencesOfOpenReviewsAreGuarded(t *testing.T) {
	t.Parallel()

	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			t.Parallel()

			n := startNode(t, coretest.Open(t, coretest.Options{Dialect: dialect}))
			s := n.newSeed(t)
			feature := n.commit(t, s, s.base, "feature", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "feature\n"})
			_, err := gitalypb.NewReviewServiceClient(n.conn).CreateReview(n.ctx, &gitalypb.CreateReviewRequest{
				Repository:   s.repo,
				TargetBranch: []byte("main"),
				HeadCommitId: feature.String(),
			})
			require.NoError(t, err)

			ref := corestore.KeepAroundRef(feature.String())
			require.Equal(t, corestore.Target(feature.String()), n.dbRefs(t, s.repo)[ref])
			before := n.dbRefs(t, s.repo)

			refs := gitalypb.NewRefServiceClient(n.conn)
			_, err = refs.DeleteRefs(n.ctx, &gitalypb.DeleteRefsRequest{Repository: s.repo, Refs: [][]byte{[]byte(ref)}})
			require.Error(t, err, "DeleteRefs")
			require.Equal(t, before, n.dbRefs(t, s.repo))

			_, err = gitalypb.NewRepositoryServiceClient(n.conn).WriteRef(n.ctx, &gitalypb.WriteRefRequest{
				Repository: s.repo,
				Ref:        []byte(ref),
				Revision:   []byte(s.base.String()),
			})
			require.Error(t, err, "WriteRef")
			require.Equal(t, before, n.dbRefs(t, s.repo))

			stream, err := refs.UpdateReferences(n.ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UpdateReferencesRequest{
				Repository: s.repo,
				Updates: []*gitalypb.UpdateReferencesRequest_Update{{
					Reference:   []byte(ref),
					OldObjectId: []byte(feature.String()),
					NewObjectId: []byte(gittest.DefaultObjectHash.ZeroOID.String()),
				}},
			}))
			_, err = stream.CloseAndRecv()
			require.Error(t, err, "UpdateReferences")
			require.Equal(t, before, n.dbRefs(t, s.repo))

			n.requireSameRefs(t, s.repo)
			require.True(t, hasObject(t, n, s.repo, feature))

			// A keep-around reference that no open review uses is not guarded.
			other := n.commit(t, s, s.base, "other", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "other\n"})
			loose := corestore.KeepAroundRef(other.String())
			_, err = gitalypb.NewRepositoryServiceClient(n.conn).WriteRef(n.ctx, &gitalypb.WriteRefRequest{
				Repository: s.repo,
				Ref:        []byte(loose),
				Revision:   []byte(other.String()),
			})
			require.NoError(t, err)
			require.Contains(t, n.dbRefs(t, s.repo), loose)
			_, err = refs.DeleteRefs(n.ctx, &gitalypb.DeleteRefsRequest{Repository: s.repo, Refs: [][]byte{[]byte(loose)}})
			require.NoError(t, err)
			require.NotContains(t, n.dbRefs(t, s.repo), loose)
		})
	}
}

// TestOpenReviewCommitsSurviveHousekeeping: an open review's commit whose
// branch was deleted survives the object housekeeping of every node, also of
// a node that hydrated the repository after the review was created
// (doc/graft/PLAN7.md, REVIEW ROOTS amendment).
func TestOpenReviewCommitsSurviveHousekeeping(t *testing.T) {
	t.Parallel()

	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			t.Parallel()

			databaseURL := coretest.DatabaseURL(t, dialect)
			bucketURL := coretest.BucketURL(t, false)
			a := startNode(t, coretest.Open(t, coretest.Options{Dialect: dialect, DatabaseURL: databaseURL, BucketURL: bucketURL, NodeID: "node-a"}))

			s := a.newSeed(t)
			feature := a.commit(t, s, s.base, "feature", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "feature\n"})
			a.createBranch(t, s.repo, "feature", feature)
			created, err := gitalypb.NewReviewServiceClient(a.conn).CreateReview(a.ctx, &gitalypb.CreateReviewRequest{
				Repository:   s.repo,
				TargetBranch: []byte("main"),
				HeadCommitId: feature.String(),
			})
			require.NoError(t, err)
			a.deleteBranch(t, s.repo, "feature")

			housekeep(t, a, s.repo)
			require.True(t, hasObject(t, a, s.repo, feature), "node A keeps the commit of the open review")

			// Node B hydrates after the review was created.
			b := startNode(t, coretest.Open(t, coretest.Options{Dialect: dialect, DatabaseURL: databaseURL, BucketURL: bucketURL, NodeID: "node-b"}))
			repoB := &gitalypb.Repository{StorageName: b.cfg.Storages[0].Name, RelativePath: s.repo.GetRelativePath()}
			got, err := gitalypb.NewReviewServiceClient(b.conn).GetReview(b.ctx, &gitalypb.GetReviewRequest{Repository: repoB, Id: created.GetReview().GetId()})
			require.NoError(t, err)
			require.Equal(t, feature.String(), got.GetReview().GetHeadCommitId())

			housekeep(t, b, repoB)
			require.True(t, hasObject(t, b, repoB, feature), "node B keeps the commit of the open review")
		})
	}
}
