package gc

func SetFaultHook(c *Collector, hook func(point string) error) { c.faultHook = hook }

// ReconcileBatch is reconcileBatch, for the tests.
const ReconcileBatch = reconcileBatch
