package repository

import (
	"context"

	"github.com/feder-cr/open_scion/internal/gitaly/repoutil"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

func (s *server) RemoveRepository(ctx context.Context, in *gitalypb.RemoveRepositoryRequest) (*gitalypb.RemoveRepositoryResponse, error) {
	repository := in.GetRepository()
	if err := s.locator.ValidateRepository(ctx, repository); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	if err := repoutil.Remove(ctx, s.logger, s.locator, s.txManager, s.repositoryCounter, repository); err != nil {
		return nil, err
	}

	if tx := storage.ExtractTransaction(ctx); tx != nil {
		if err := s.migrationStateManager.RecordKeyDeletion(tx, tx.OriginalRepository(repository).GetRelativePath()); err != nil {
			return nil, structerr.NewInternal("recording migration key: %w", err)
		}
	}

	return &gitalypb.RemoveRepositoryResponse{}, nil
}
