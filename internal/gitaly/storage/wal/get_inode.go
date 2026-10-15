package wal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// GetInode gets the inode of a file system object at the given path.
// If the file does not exist, zero is returned as the inode number with
// no error raised.
func GetInode(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}

		return 0, fmt.Errorf("stat: %w", err)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unexpected stat type: %w", err)
	}

	return stat.Ino, nil
}
