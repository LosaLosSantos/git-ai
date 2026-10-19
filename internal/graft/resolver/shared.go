package resolver

import "github.com/feder-cr/git-ai/internal/gitaly/config"

// Shared is the resolver client of the process: built once from the configuration and shared by
// the operations servers of every listener, so that the check slots bound the checks of the whole
// process and a configuration that cannot be built gives one error.
type Shared struct {
	// Client is nil when resolution is disabled or Err is set.
	Client *Client
	// Err is why the configured resolver cannot be built, if it cannot.
	Err error
	// Verify is the pool of verifications of the process (PLAN10 15.9). It is set whenever
	// verify.enabled is, with or without an AI resolver: an agent's resolution and the rules' are
	// verified too, out of the lane, and the pool bounds all of them together.
	Verify *VerifyPool
}

// NewShared builds the resolver client of the process from cfg.
func NewShared(cfg config.GraftResolver) *Shared {
	var pool *VerifyPool
	if cfg.Verify.Enabled {
		pool = NewVerifyPool(cfg)
	}
	client, err := New(cfg)
	if err != nil {
		return &Shared{Err: err, Verify: pool}
	}
	if client != nil && pool != nil {
		client.verify = pool
	}
	return &Shared{Client: client, Verify: pool}
}
