package operations

import (
	"context"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/quarantine"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

func (s *Server) updateReferenceWithHooks(
	ctx context.Context,
	repo *gitalypb.Repository,
	user *gitalypb.User,
	quarantine *quarantine.Dir,
	reference git.ReferenceName,
	newrev, oldrev git.ObjectID,
	pushOptions ...string,
) error {
	return s.updater.UpdateReference(ctx, repo, user, quarantine, reference, newrev, oldrev, pushOptions...)
}
