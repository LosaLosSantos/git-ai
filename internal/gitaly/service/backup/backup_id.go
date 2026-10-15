package backup

import (
	"context"
	"errors"

	"github.com/feder-cr/open_scion/internal/backup"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// WriteBackupID writes a backup ID marker to the configured backup sink.
func (s *server) WriteBackupID(ctx context.Context, in *gitalypb.WriteBackupIDRequest) (*gitalypb.WriteBackupIDResponse, error) {
	if s.backupSink == nil {
		return nil, structerr.NewFailedPrecondition("write backup ID: server-side backups are not configured")
	}

	if in.GetBackupId() == "" {
		return nil, structerr.NewInvalidArgument("empty backup ID")
	}

	manager := backup.NewIDManager(s.backupSink)

	if err := manager.WriteBackupID(ctx, &backup.WriteBackupIDRequest{
		BackupID:   in.GetBackupId(),
		PathPrefix: in.GetPathPrefix(),
	}); err != nil {
		return nil, structerr.NewInternal("write backup id: %w", err)
	}

	return &gitalypb.WriteBackupIDResponse{}, nil
}

// ReadLatestBackupID returns the ID of the most recently completed backup run.
func (s *server) ReadLatestBackupID(ctx context.Context, in *gitalypb.ReadLatestBackupIDRequest) (*gitalypb.ReadLatestBackupIDResponse, error) {
	if s.backupSink == nil {
		return nil, structerr.NewFailedPrecondition("read latest backup ID: server-side backups are not configured")
	}

	manager := backup.NewIDManager(s.backupSink)

	backupID, err := manager.ReadLatestBackupID(ctx, in.GetPathPrefix())
	if err != nil {
		if errors.Is(err, backup.ErrDoesntExist) {
			return nil, structerr.NewNotFound("no backup id markers found: %w", err)
		}
		return nil, structerr.NewInternal("read latest backup id: %w", err)
	}

	return &gitalypb.ReadLatestBackupIDResponse{
		BackupId: backupID,
	}, nil
}
