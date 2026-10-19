package localrepo

// SetTestHook installs a function called at the named points of a write of w.
func (w *TreeWriter) SetTestHook(hook func(point string)) {
	w.testHook = hook
}
