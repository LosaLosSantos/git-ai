package syncer

// SetTestHook installs a function called at the named points of a catch-up
// ("fetched", "checked"). Only for tests; set it before the first EnsureFresh.
func (s *Syncer) SetTestHook(hook func(point string)) {
	s.testHook = hook
}
