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
	l, answer, err := s.landResolveAt(ctx, in, repo, tip, commitID, committer, landVerdicts{})
	if err != nil {
		return landResolution{}, err
	}
	res, err := s.landWriteResolution(ctx, in, repo, tip, commitID, l.mergedTree, committer, identity.Actor(ctx, ""), answer, "")
	return res, l.verifyRejected(err)
}
