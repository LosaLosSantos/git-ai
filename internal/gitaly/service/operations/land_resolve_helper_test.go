package operations

import (
	"context"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// resolveLandConflicts resolves the conflicts of tip and commitID and writes the merge commit when
// the land is landable: the pre-resolution (landResolveAt) and the lane's write in one call, as the
// tests of the resolution itself need it. UserLand runs the two apart, the first out of the lane.
func (s *Server) resolveLandConflicts(ctx context.Context, in *gitalypb.UserLandRequest, repo *localrepo.Repo, tip, commitID git.ObjectID, committer git.Signature) (landResolution, error) {
	pre := newLandPreResolution()
	l, answer, err := s.landResolveAt(ctx, in, repo, tip, commitID, committer, pre)
	if err != nil {
		return landResolution{}, err
	}
	// As the lane does for a model's resolution: the tree is the one verify.command passed on, out
	// of the lane, or the answer would not have been landable.
	var tree git.ObjectID
	if landAnswerProposes(answer) || s.resolveCfg.Verify.Enabled {
		if tree, err = s.landResolvedTree(ctx, repo, l.mergedTree, answer); err != nil {
			return landResolution{}, err
		}
	}
	return s.landWriteResolution(ctx, in, repo, tip, commitID, l.mergedTree, committer, identity.Actor(ctx, ""), answer, "", tree, pre.isVerified(tree))
}
