package syncer

import (
	"context"
	"os"
)

// CleanBuild runs the cleanup of one build directory, as CleanBuilds does for
// each.
func CleanBuild(s *Syncer, dir string) (bool, error) {
	return s.cleanBuild(dir)
}

// LockBuild takes the lock of the build directory dir as a build does, calling
// afterOpen between the open of the lock file and its flock.
func LockBuild(dir string, afterOpen func(path string)) (*os.File, error) {
	return lockBuild(dir, afterOpen)
}

// SetTestHook installs a function called at the named points of a catch-up
// ("fetched", "checked") and of a seed ("seed" before the pack is indexed,
// "seeded" after, "installed" after the install). Only for tests; set it
// before the first EnsureFresh.
func (s *Syncer) SetTestHook(hook func(point string)) {
	s.testHook = hook
}

// SetRoundLimit ends every sync round after n changesets, so that tests get
// partial rounds deterministically. Set it before the first EnsureFresh.
func (s *Syncer) SetRoundLimit(n int) {
	s.roundLimit = n
}

// DurableStep runs one durable step of the repository at path, as the
// durable-step loop does every interval (final: as the stop does).
func (s *Syncer) DurableStep(ctx context.Context, path string, final bool) {
	s.durableStep(ctx, path, final)
}
