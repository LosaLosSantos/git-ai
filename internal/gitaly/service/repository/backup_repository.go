package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/backup"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// BackupRepository creates a full or incremental backup streamed directly to object-storage.
func (s *server) BackupRepository(ctx context.Context, in *gitalypb.BackupRepositoryRequest) (*gitalypb.BackupRepositoryResponse, error) {
	if s.backupSink == nil || s.backupLocator == nil {
		return nil, structerr.NewFailedPrecondition("backup repository: server-side backups are not configured")
	}
	if err := s.validateBackupRepositoryRequest(ctx, in); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	manager := backup.NewManagerLocal(
		s.backupSink,
		s.logger,
		s.backupLocator,
		s.locator,
		s.gitCmdFactory,
		s.catfileCache,
		s.txManager,
		s.repositoryCounter,
		s.migrationStateManager,
	)

	err := manager.Create(ctx, &backup.CreateRequest{
		Repository:       in.GetRepository(),
		VanityRepository: in.GetVanityRepository(),
		BackupID:         in.GetBackupId(),
		LatestBackupID:   in.GetLatestBackupId(),
		PathPrefix:       in.GetPathPrefix(),
		Incremental:      in.GetIncremental(),
	})

	switch {
	case errors.Is(err, backup.ErrSkipped):
		return nil, structerr.NewFailedPrecondition("backup repository: %w", err).WithDetail(
			&gitalypb.BackupRepositoryResponse_SkippedError{},
		)
	case err != nil:
		return nil, structerr.NewInternal("backup repository: %w", err)
	}

	return &gitalypb.BackupRepositoryResponse{}, nil
}

func (s *server) validateBackupRepositoryRequest(ctx context.Context, in *gitalypb.BackupRepositoryRequest) error {
	if in.GetBackupId() == "" {
		return fmt.Errorf("empty BackupId")
	}
	if err := s.locator.ValidateRepository(ctx, in.GetRepository(), storage.WithSkipRepositoryExistenceCheck()); err != nil {
		return err
	}
	if err := s.locator.ValidateRepository(ctx, in.GetVanityRepository(),
		storage.WithSkipStorageExistenceCheck(),
	); err != nil {
		return err
	}
	if err := backup.ValidatePathPrefix(in.GetPathPrefix()); err != nil {
		return err
	}

	return nil
}
