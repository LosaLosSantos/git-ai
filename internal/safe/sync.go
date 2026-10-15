package safe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/trace"
)

type file interface {
	Sync() error
	Close() error
}

// Syncer implements helper methods for fsyncing files and directories.
type Syncer struct {
	// open is used by the Sync* functions to open files for syncing. By default, Syncer opens
	// files via the `os` package as usual. This is parametrized only so we can test the correct
	// calls were made as we don't have good way to test the fsyncing otherwise.
	open func(string) (file, error)
}

// NewSyncer returns a new Syncer.
func NewSyncer() Syncer {
	return Syncer{
		open: func(path string) (file, error) {
			return os.Open(path)
		},
	}
}

// Sync opens the file/directory at the given path and syncs it. Syncing a directory syncs just
// the directory itself, not the children. If the children need to be synced as well, use SyncRecursive.
func (s Syncer) Sync(ctx context.Context, path string) error {
	defer trace.StartRegion(ctx, "Sync").End()

	f, err := s.open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}

	return nil
}

// SyncParent syncs the parent directory of the given path. The path is cleaned prior to determining
// the parent
func (s Syncer) SyncParent(ctx context.Context, path string) error {
	defer trace.StartRegion(ctx, "SyncParent").End()
	return s.Sync(ctx, filepath.Dir(filepath.Clean(path)))
}

// SyncRecursive walks the file tree rooted at path and fsyncing the root and the children.
func (s Syncer) SyncRecursive(ctx context.Context, path string) error {
	defer trace.StartRegion(ctx, "SyncRecursive").End()
	return filepath.WalkDir(path, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		return s.Sync(ctx, path)
	})
}

// SyncHierarchy walks the hierarchy from <rootPath>/<relativePath> up to the
// rootPath and syncs everything on the way. If the path points to a directory,
// it only syncs from the directory and upwards, not the contents of the directory.
func (s Syncer) SyncHierarchy(ctx context.Context, rootPath, relativePath string) error {
	defer trace.StartRegion(ctx, "SyncHierarchy").End()
	// Traverse the directory hierarchy all the way up to root and fsync them.
	currentPath := filepath.Join(rootPath, relativePath)
	for {
		if err := s.Sync(ctx, currentPath); err != nil {
			return fmt.Errorf("sync: %w", err)
		}

		if currentPath == rootPath {
			break
		}

		currentPath = filepath.Dir(currentPath)
	}

	return nil
}
