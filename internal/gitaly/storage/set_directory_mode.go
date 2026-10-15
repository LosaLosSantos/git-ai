package storage

import (
	"io/fs"
	"os"
	"path/filepath"
)

// SetDirectoryMode walks the directory hierarchy at path and sets each directory's mode to the given mode.
func SetDirectoryMode(path string, mode fs.FileMode) error {
	return filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			return nil
		}

		return os.Chmod(path, mode)
	})
}
