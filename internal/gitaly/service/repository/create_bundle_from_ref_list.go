package repository

import (
	"bytes"
	"errors"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
)

func (s *server) CreateBundleFromRefList(stream gitalypb.RepositoryService_CreateBundleFromRefListServer) error {
	ctx := stream.Context()

	firstRequest, err := stream.Recv()
	if err != nil {
		return err
	}

	repository := firstRequest.GetRepository()
	if err := s.locator.ValidateRepository(ctx, repository); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	repo := s.localRepoFactory.Build(repository)

	if err := housekeeping.CleanupWorktrees(ctx, repo); err != nil {
		return structerr.NewInternal("cleaning up worktrees: %w", err)
	}

	// The patterns go to git-bundle(1) one per line on its stdin: a pattern with a line break or a NUL
	// would be another pattern, or cut short (git.ValidateStdinRevision). The first request is checked
	// before anything starts; a later one is checked as it is read, and its refusal is the answer
	// whatever the command made of the stream.
	var invalidPattern error
	checkPatterns := func(request *gitalypb.CreateBundleFromRefListRequest) error {
		for _, pattern := range request.GetPatterns() {
			if err := git.ValidateStdinRevision(string(pattern)); err != nil {
				return structerr.NewInvalidArgument("invalid pattern: %w", err)
			}
		}
		return nil
	}
	if err := checkPatterns(firstRequest); err != nil {
		return err
	}
	firstRead := true
	patterns := streamio.NewReader(func() ([]byte, error) {
		var request *gitalypb.CreateBundleFromRefListRequest
		if firstRead {
			firstRead = false
			request = firstRequest
		} else {
			var err error
			request, err = stream.Recv()
			if err != nil {
				return nil, err
			}
			if err := checkPatterns(request); err != nil {
				invalidPattern = err
				return nil, err
			}
		}
		return append(bytes.Join(request.GetPatterns(), []byte("\n")), '\n'), nil
	})
	writer := streamio.NewWriter(func(p []byte) error {
		return stream.Send(&gitalypb.CreateBundleFromRefListResponse{Data: p})
	})

	err = repo.CreateBundle(ctx, writer, &localrepo.CreateBundleOpts{Patterns: patterns})
	switch {
	case invalidPattern != nil:
		return invalidPattern
	case errors.Is(err, localrepo.ErrEmptyBundle):
		return structerr.NewFailedPrecondition("%w", err)
	case err != nil:
		return structerr.NewInternal("%w", err)
	}

	return nil
}
