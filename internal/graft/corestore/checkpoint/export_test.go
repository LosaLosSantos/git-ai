package checkpoint

// SetFaultHook sets the hook called at the fault points of c's builds.
func SetFaultHook(c *Checkpointer, hook func(point string) error) { c.faultHook = hook }
