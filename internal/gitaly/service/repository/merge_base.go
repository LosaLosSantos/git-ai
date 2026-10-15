package repository

import (
	"context"
	"io"

	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/helper/text"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

func (s *server) FindMergeBase(ctx context.Context, req *gitalypb.FindMergeBaseRequest) (*gitalypb.FindMergeBaseResponse, error) {
	repoProto := req.GetRepository()
	if err := s.locator.ValidateRepository(ctx, repoProto); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	var revisions []string
	for _, rev := range req.GetRevisions() {
		revisions = append(revisions, string(rev))
	}

	if len(revisions) < 2 {
		return nil, structerr.NewInvalidArgument("at least 2 revisions are required")
	}

	repo := s.localRepoFactory.Build(repoProto)

	cmd, err := repo.Exec(ctx,
		gitcmd.Command{
			Name: "merge-base",
			Args: revisions,
		},
		gitcmd.WithSetupStdout(),
	)
	if err != nil {
		return nil, structerr.NewInternal("cmd: %w", err)
	}

	mergeBase, err := io.ReadAll(cmd)
	if err != nil {
		return nil, err
	}

	mergeBaseStr := text.ChompBytes(mergeBase)

	if err := cmd.Wait(); err != nil {
		// On error just return an empty merge base
		return &gitalypb.FindMergeBaseResponse{Base: ""}, nil
	}

	return &gitalypb.FindMergeBaseResponse{Base: mergeBaseStr}, nil
}
