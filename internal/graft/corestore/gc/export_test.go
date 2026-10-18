package gc

// SetFaultHook sets the hook called at the fault points of c's runs.
func SetFaultHook(c *Collector, hook func(point string) error) { c.faultHook = hook }
