package backup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/feder-cr/open_scion/internal/gitaly/storage"
)

// ManifestLocator locates backup paths based on manifest files that are
// written to a predetermined path:
//
//	manifests/<repo_storage_name>/<repo_relative_path>/<backup_id>.toml
//
// It relies on Fallback to determine paths of new backups.
type ManifestLocator struct {
	Loader ManifestLoader
}

// NewLocator builds a new ManifestLocator.
func NewLocator(sink *Sink) ManifestLocator {
	return ManifestLocator{
		Loader: NewManifestLoader(sink),
	}
}

// BeginFull returns a tentative first step needed to create a new full backup.
func (l ManifestLocator) BeginFull(ctx context.Context, repo storage.Repository, backupID string, opts ...Option) *Backup {
	cfg := applyOptions(opts)
	storageName := repo.GetStorageName()
	relativePath := repo.GetRelativePath()

	return &Backup{
		ID:         backupID,
		Repository: repo,
		Steps: []Step{
			{
				BundlePath:      filepath.Join(cfg.pathPrefix, storageName, relativePath, backupID, "001.bundle"),
				RefPath:         filepath.Join(cfg.pathPrefix, storageName, relativePath, backupID, "001.refs"),
				CustomHooksPath: filepath.Join(cfg.pathPrefix, storageName, relativePath, backupID, "001.custom_hooks.tar"),
			},
		},
	}
}

// BeginIncremental returns a tentative step needed to create a new incremental
// backup. The incremental backup is always based off of the latest backup. If
// there is no latest backup, a new full backup step is returned using backupID.
func (l ManifestLocator) BeginIncremental(ctx context.Context, repo storage.Repository, backupID string, opts ...Option) (*Backup, error) {
	cfg := applyOptions(opts)
	var backup *Backup
	var err error
	if cfg.latestBackupID != "" {
		backup, err = l.Find(ctx, repo, cfg.latestBackupID, opts...)
		if errors.Is(err, ErrDoesntExist) {
			// This repo wasn't part of the backup run identified by
			// latestBackupID (e.g. partial failure or newly added repo).
			// Fall back to per-repo latest manifest.
			backup, err = l.Loader.ReadLatestManifest(ctx, repo, opts...)
		}
	} else {
		backup, err = l.Loader.ReadLatestManifest(ctx, repo, opts...)
	}

	switch {
	case errors.Is(err, ErrDoesntExist):
		return l.BeginFull(ctx, repo, backupID, opts...), nil
	case err != nil:
		return nil, fmt.Errorf("manifest: begin incremental: %w", err)
	}

	// If the previous backup recorded the repository as non-existent, no ref
	// blob was ever written for it, so there is nothing to base an increment on.
	// Treat the repository as new and write a full backup.
	if backup.NonExistent {
		return l.BeginFull(ctx, repo, backupID), nil
	}

	// The NonExistent check above does not cover manifests that were already
	// corrupted in object storage before NonExistent was tracked, nor ref blobs
	// that were lost from the sink. In those cases the manifest still points at
	// a ref blob that cannot be read, so fall back to a full backup to self-heal
	// rather than failing every subsequent incremental.
	if len(backup.Steps) > 0 {
		previousRefPath := backup.Steps[len(backup.Steps)-1].RefPath
		exists, err := l.Loader.Exists(ctx, previousRefPath)
		if err != nil {
			return nil, fmt.Errorf("manifest: begin incremental: check previous ref: %w", err)
		}
		if !exists {
			return l.BeginFull(ctx, repo, backupID), nil
		}
	}

	storageName := repo.GetStorageName()
	relativePath := repo.GetRelativePath()
	n := len(backup.Steps) + 1

	// This is a convenience that could be calculated but it is cheap enough to
	// generate here. It means that the increment generating code only needs to
	// refer to a single step.
	var previousRefPath string
	if len(backup.Steps) > 0 {
		previousRefPath = backup.Steps[len(backup.Steps)-1].RefPath
	}

	backup.ID = backupID
	backup.Steps = append(backup.Steps, Step{
		BundlePath:      filepath.Join(cfg.pathPrefix, storageName, relativePath, backupID, fmt.Sprintf("%03d.bundle", n)),
		RefPath:         filepath.Join(cfg.pathPrefix, storageName, relativePath, backupID, fmt.Sprintf("%03d.refs", n)),
		PreviousRefPath: previousRefPath,
		CustomHooksPath: filepath.Join(cfg.pathPrefix, storageName, relativePath, backupID, fmt.Sprintf("%03d.custom_hooks.tar", n)),
	})

	return backup, nil
}

// Commit passes through to Fallback, then writes a manifest file for the backup.
func (l ManifestLocator) Commit(ctx context.Context, backup *Backup, opts ...Option) error {
	if err := l.Loader.WriteManifest(ctx, backup, backup.ID, opts...); err != nil {
		return fmt.Errorf("manifest: commit: %w", err)
	}

	return nil
}

// FindLatest loads the latest manifest for the provided repo using timestamp order.
func (l ManifestLocator) FindLatest(ctx context.Context, repo storage.Repository, opts ...Option) (*Backup, error) {
	backup, err := l.Loader.ReadLatestManifest(ctx, repo, opts...)
	if err != nil {
		return nil, fmt.Errorf("manifest: find latest: %w", err)
	}

	return backup, nil
}

// Find loads the manifest for the provided repo and backupID.
func (l ManifestLocator) Find(ctx context.Context, repo storage.Repository, backupID string, opts ...Option) (*Backup, error) {
	backup, err := l.Loader.ReadManifest(ctx, repo, backupID, opts...)
	if err != nil {
		return nil, fmt.Errorf("manifest: find: %w", err)
	}

	return backup, nil
}
