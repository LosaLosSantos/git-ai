package repository

import (
	"fmt"

	"github.com/feder-cr/git-ai/internal/gitaly/repoutil"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
)

// GetCustomHooks fetches the git hooks for a repository. The hooks are sent in
// a tar archive containing a `custom_hooks` directory. If no hooks are present
// in the repository, the response will have no data.
func (s *server) GetCustomHooks(in *gitalypb.GetCustomHooksRequest, stream gitalypb.RepositoryService_GetCustomHooksServer) error {
	ctx := stream.Context()

	if err := s.locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	writer := streamio.NewWriter(func(p []byte) error {
		return stream.Send(&gitalypb.GetCustomHooksResponse{Data: p})
	})

	repoPath, err := s.locator.GetRepoPath(ctx, in.GetRepository())
	if err != nil {
		return fmt.Errorf("get repo path: %w", err)
	}

	if err := repoutil.GetCustomHooks(ctx, s.logger, repoPath, writer); err != nil {
		return structerr.NewInternal("reading custom hooks: %w", err)
	}

	return nil
}

// BackupCustomHooks fetches the git hooks for a repository. The hooks are sent
// in a tar archive containing a `custom_hooks` directory. If no hooks are
// present in the repository, the response will have no data.
func (s *server) BackupCustomHooks(in *gitalypb.BackupCustomHooksRequest, stream gitalypb.RepositoryService_BackupCustomHooksServer) error {
	ctx := stream.Context()

	if err := s.locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	writer := streamio.NewWriter(func(p []byte) error {
		return stream.Send(&gitalypb.BackupCustomHooksResponse{Data: p})
	})

	repoPath, err := s.locator.GetRepoPath(ctx, in.GetRepository())
	if err != nil {
		return fmt.Errorf("get repo path: %w", err)
	}

	if err := repoutil.GetCustomHooks(ctx, s.logger, repoPath, writer); err != nil {
		return structerr.NewInternal("reading custom hooks: %w", err)
	}

	return nil
}
