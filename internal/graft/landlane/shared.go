package landlane

import (
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Shared holds the one Registry of a process. Gitaly registers its services once per listener, so
// one process runs several servers of a service. If each of them had its own Registry, requests for
// the same key that arrive on different listeners would be batched in different lanes, and the key
// would run several batches at a time. The servers therefore take their Registry from one Shared,
// which builds it on first use.
//
// Shared implements prometheus.Collector with the metrics of its Registry (see PrometheusObserver).
// Register it once, and close it when the process shuts down.
type Shared struct {
	cfg Config
	obs *PrometheusObserver

	mu     sync.Mutex
	closed bool
	// registry is the *Registry[Req, Res] built by the first SharedRegistry call.
	registry any
	// closeRegistry closes registry.
	closeRegistry func()
	// expireRegistry forgets the idle keys of registry (Registry.ExpireIdleKeys).
	expireRegistry func()
}

// NewShared creates the shared lanes of a process. The configuration is validated when the Registry
// is built.
func NewShared(cfg Config) *Shared {
	return &Shared{
		cfg: cfg,
		obs: NewPrometheusObserver(),
	}
}

// SharedRegistry returns the Registry of s. The first call builds it with exec, and every later call
// returns the same Registry and ignores exec: the callers of one Shared must pass equivalent
// executors, as any of them may run the batches of all callers. A call with other request or result
// types than the first one is an error. After Close, SharedRegistry returns ErrClosed.
func SharedRegistry[Req, Res any](s *Shared, exec Executor[Req, Res]) (*Registry[Req, Res], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, ErrClosed
	}

	if s.registry != nil {
		registry, ok := s.registry.(*Registry[Req, Res])
		if !ok {
			return nil, fmt.Errorf("shared land lanes hold a %T, not a %T", s.registry, registry)
		}

		return registry, nil
	}

	registry, err := NewRegistry(s.cfg, exec, s.obs)
	if err != nil {
		return nil, err
	}

	s.registry = registry
	s.closeRegistry = registry.Close
	s.expireRegistry = registry.ExpireIdleKeys

	return registry, nil
}

// Close closes the Registry of s, if it was built, as Registry.Close does: queued requests fail
// with ErrClosed, and Close waits for the running batches.
func (s *Shared) Close() {
	s.mu.Lock()
	s.closed = true
	closeRegistry := s.closeRegistry
	s.mu.Unlock()

	if closeRegistry != nil {
		closeRegistry()
	}
}

// Describe implements prometheus.Collector.
func (s *Shared) Describe(ch chan<- *prometheus.Desc) {
	s.obs.Describe(ch)
}

// Collect implements prometheus.Collector. It first forgets the keys of the Registry that have been
// idle for KeyStateTTL, so that the effective max size gauge does not keep keys that are no longer
// used while no request arrives.
func (s *Shared) Collect(ch chan<- prometheus.Metric) {
	s.mu.Lock()
	expire := s.expireRegistry
	s.mu.Unlock()

	if expire != nil {
		expire()
	}

	s.obs.Collect(ch)
}
