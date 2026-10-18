package syncer

import "context"

// SetTestHook installs a function called at the named points of a catch-up
// ("fetched", "checked"). Only for tests; set it before the first EnsureFresh.
func (s *Syncer) SetTestHook(hook func(point string)) {
	s.testHook = hook
}

// DurableStep runs one durable step of the repository at path, as
// RunDurableSteps does every interval.
func (s *Syncer) DurableStep(ctx context.Context, path string) {
	s.durableStep(ctx, path)
}
