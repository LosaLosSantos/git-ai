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
}

// NewShared builds the resolver client of the process from cfg.
func NewShared(cfg config.GraftResolver) *Shared {
	client, err := New(cfg)
	if err != nil {
		return &Shared{Err: err}
	}
	return &Shared{Client: client}
}
