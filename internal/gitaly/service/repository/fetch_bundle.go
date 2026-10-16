package repository

import (
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
)

func (s *server) FetchBundle(stream gitalypb.RepositoryService_FetchBundleServer) error {
	ctx := stream.Context()

	firstRequest, err := stream.Recv()
	if err != nil {
		return structerr.NewInternal("first request: %w", err)
	}

	if err := s.locator.ValidateRepository(ctx, firstRequest.GetRepository()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	firstRead := true
	reader := streamio.NewReader(func() ([]byte, error) {
		if firstRead {
			firstRead = false
			return firstRequest.GetData(), nil
		}

		request, err := stream.Recv()
		return request.GetData(), err
	})

	repo := s.localRepoFactory.Build(firstRequest.GetRepository())

	// Verify that the repository actually exists.
	if _, err := repo.Path(ctx); err != nil {
		return err
	}

	opts := &localrepo.FetchBundleOpts{
		UpdateHead: firstRequest.GetUpdateHead(),
	}

	if err := repo.FetchBundle(ctx, s.txManager, reader, opts); err != nil {
		return structerr.NewInternal("%w", err)
	}

	return stream.SendAndClose(&gitalypb.FetchBundleResponse{})
}
