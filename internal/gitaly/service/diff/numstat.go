package diff

import (
	"context"
	"errors"
	"io"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/diff"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

var maxNumStatBatchSize = 1000

func (s *server) DiffStats(in *gitalypb.DiffStatsRequest, stream gitalypb.DiffService_DiffStatsServer) error {
	if err := validateRequest(stream.Context(), s.locator, in); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	repo := s.localRepoFactory.Build(in.GetRepository())

	var flags []gitcmd.Option
	switch in.GetWhitespaceChanges() {
	case gitalypb.DiffStatsRequest_WHITESPACE_CHANGES_IGNORE_ALL:
		flags = append(flags, gitcmd.Flag{Name: "--ignore-all-space"})
	case gitalypb.DiffStatsRequest_WHITESPACE_CHANGES_IGNORE:
		flags = append(flags, gitcmd.Flag{Name: "--ignore-space-change"})
	}

	var batch []*gitalypb.DiffStats
	if err := numstat(stream.Context(), repo, in.GetLeftCommitId(), in.GetRightCommitId(), nil, flags, func(stat diff.NumStat) error {
		batch = append(batch, &gitalypb.DiffStats{
			Additions: stat.Additions,
			Deletions: stat.Deletions,
			Path:      stat.Path,
			OldPath:   stat.OldPath,
		})

		if len(batch) == maxNumStatBatchSize {
			if err := sendStats(batch, stream); err != nil {
				return err
			}

			batch = nil
		}

		return nil
	}); err != nil {
		return err
	}

	return sendStats(batch, stream)
}

func sendStats(batch []*gitalypb.DiffStats, stream gitalypb.DiffService_DiffStatsServer) error {
	if len(batch) == 0 {
		return nil
	}

	if err := stream.Send(&gitalypb.DiffStatsResponse{Stats: batch}); err != nil {
		return structerr.NewInternal("send: %w", err)
	}

	return nil
}

func numstat(ctx context.Context, repo *localrepo.Repo, leftCommitID, rightCommitID string, paths []string, flags []gitcmd.Option, cb func(diff.NumStat) error) error {
	cmd, err := repo.Exec(ctx, gitcmd.Command{
		Name:        "diff",
		Flags:       append([]gitcmd.Option{gitcmd.Flag{Name: "--numstat"}, gitcmd.Flag{Name: "-z"}}, flags...),
		Args:        []string{leftCommitID, rightCommitID},
		PostSepArgs: paths,
	}, gitcmd.WithSetupStdout())
	if err != nil {
		return structerr.NewInternal("cmd: %w", err)
	}

	parser := diff.NewDiffNumStatParser(cmd)

	for {
		stat, err := parser.NextNumStat()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return err
		}

		if err := cb(*stat); err != nil {
			return err
		}
	}

	if err := cmd.Wait(); err != nil {
		return structerr.NewFailedPrecondition("%w", err)
	}

	return nil
}
