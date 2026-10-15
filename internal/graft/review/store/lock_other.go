//go:build !unix

package store

// lockFile has no cross-process lock on this platform: the in-process mutex is
// the only protection. Gitaly servers run on unix systems.
func lockFile(string) (func(), error) {
	return func() {}, nil
}

// syncDir is a no-op on this platform, where directories cannot be fsynced.
func syncDir(string) error {
	return nil
}
