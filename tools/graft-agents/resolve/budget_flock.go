//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package resolve

import (
	"fmt"
	"os"
	"syscall"
)

func checkFlock() error { return nil }

// lockFile takes an exclusive flock on file, creating it, and returns the unlock function.
func lockFile(file string) (func(), error) {
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("budget: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("budget: locking %s: %w", file, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
