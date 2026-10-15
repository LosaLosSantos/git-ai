package repository

import (
	"context"
	"errors"

	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

func (s *server) RepositoryExists(ctx context.Context, in *gitalypb.RepositoryExistsRequest) (*gitalypb.RepositoryExistsResponse, error) {
	err := s.locator.ValidateRepository(ctx, in.GetRepository())
	switch {
	case err == nil:
		return &gitalypb.RepositoryExistsResponse{Exists: true}, nil
	case errors.Is(err, storage.ErrRepositoryNotFound):
		return &gitalypb.RepositoryExistsResponse{Exists: false}, nil
	case errors.Is(err, storage.ErrRepositoryNotValid):
		// TODO: this error case should really be converted to an actual error.
		return &gitalypb.RepositoryExistsResponse{Exists: false}, nil
	default:
		return nil, err
	}
}
