package syncer

import "context"

// SetTestHook installs a function called at the named points of a catch-up
// ("fetched", "checked"). Only for tests; set it before the first EnsureFresh.
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
