package operations

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestUserLand_finalPass_aTreeThatPassedStillLands: the land has run out of time (the final pass),
// but the tree it would land passed verify.command in an earlier round, so the verification is not
// needed again and the land lands, without running the command once more.
func TestUserLand_finalPass_aTreeThatPassedStillLands(t *testing.T) {
	t.Parallel()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, modelAnswer())
	vdir, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(enabledResolver(rargv), vargv, "")
	var s *Server
	operationServerHooks.Store(t.Name(), func(served *Server) { s = served })
	fx := newResolveFixture(t, rcfg)
	require.NotNil(t, s, "the service's own Server")

	pre := newLandPreResolution()
	pre.verifyAll = true
	require.NoError(t, s.landPreResolve(fx.ctx, fx.request("Land"), fx.committer(), pre))
	require.Equal(t, 1, verifyRuns(t, vdir))

	resp, err := s.landFinalPass(fx.ctx, fx.request("Land"), fx.committer(), pre, "the time ran out")
	require.NoError(t, err, "a tree that passed is landed, not proposed")
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, git.ObjectID(resp.GetLandedCommitId()))
	require.Equal(t, 1, verifyRuns(t, vdir), "the command did not run again")
}

// TestLandPrecheckResolution_aServerFaultIsInternal: a verification that cannot run because of the
// server (here its directory cannot be made) is Internal on the precheck path, as on the resolve
// path, not Unknown.
func TestLandPrecheckResolution_aServerFaultIsInternal(t *testing.T) {
	t.Parallel()

	vdir, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(config.GraftResolver{}, vargv, "")
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	s.verifyTmpDir = t.TempDir() + "/no/such/directory"
	in := fx.request("Land")
	in.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

	pc, err := s.landPrecheckResolution(fx.ctx, in, fx.repo)
	require.Nil(t, pc)
	require.Equal(t, codes.Internal, status.Code(err), "err = %v", err)
	require.Contains(t, err.Error(), "verification directory")
	require.Equal(t, 0, verifyRuns(t, vdir))
}

// TestLandFinalPass_givingUpIsTheKeptMovingError: a land that loses every pass of the final pass ends
// as any land that lost the race: the retryable FailedPrecondition "kept moving", with the
// ReferenceUpdate detail (landKeptMovingError), not a bare error.
func TestLandFinalPass_givingUpIsTheKeptMovingError(t *testing.T) {
	t.Parallel()

	in := &gitalypb.UserLandRequest{Branch: []byte(git.DefaultBranch), CommitId: "c"}
	err := landFinalPassGaveUp(in, &landNeedsResolution{tip: "1111111111111111111111111111111111111111"}, landFinalPasses)
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "err = %v", err)
	require.Contains(t, err.Error(), fmt.Sprintf("kept moving, giving up after %d attempts", landFinalPasses))

	var detail *gitalypb.UserLandError
	for _, d := range status.Convert(err).Details() {
		if e, ok := d.(*gitalypb.UserLandError); ok {
			detail = e
		}
	}
	require.NotNil(t, detail, "the UserLandError detail of the kept-moving error")
	update := detail.GetReferenceUpdate()
	require.NotNil(t, update, "the ReferenceUpdate detail")
	require.Equal(t, "refs/heads/"+git.DefaultBranch, string(update.GetReferenceName()))
	require.Equal(t, "1111111111111111111111111111111111111111", update.GetOldOid())
	require.Empty(t, update.GetNewOid(), "where the branch moved to is not known there")
}
