package mvcc

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// cacheMetrics holds the Prometheus metrics shared by every LocalCache a
// CacheFactory hands out. A single instance lives on the factory and is set on
// each cache it creates, so all MVCC consumers report into the same metrics.
type cacheMetrics struct {
	// prepareDuration measures how long warming a repository's local cache from
	// durable storage takes, labeled by op (accessor/mutator) and status.
	prepareDuration *prometheus.HistogramVec
	// commitTotal counts manifest-pointer commit attempts by outcome.
	commitTotal *prometheus.CounterVec
}

func newCacheMetrics() *cacheMetrics {
	return &cacheMetrics{
		prepareDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "gitaly_mvcc_cache_prepare_duration_seconds",
				Help: "Latency of warming a repository's local MVCC cache from durable storage.",
				// The default buckets cap at 10s, but warming a cold repository
				// pulls manifests, reftables, and packs from durable storage and
				// can take tens of seconds. Extend the tail so those slow warms
				// stay visible instead of piling into +Inf.
				Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 60},
			},
			[]string{"op", "status"},
		),
		commitTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gitaly_mvcc_commit_total",
				Help: "Number of MVCC manifest-pointer commit attempts by outcome.",
			},
			[]string{"status"},
		),
	}
}

// Describe implements prometheus.Collector.
func (m *cacheMetrics) Describe(ch chan<- *prometheus.Desc) {
	m.prepareDuration.Describe(ch)
	m.commitTotal.Describe(ch)
}

// Collect implements prometheus.Collector.
func (m *cacheMetrics) Collect(ch chan<- prometheus.Metric) {
	m.prepareDuration.Collect(ch)
	m.commitTotal.Collect(ch)
}

// opLabel returns the "op" label value for the cache's RPC kind.
func (m *LocalCache) opLabel() string {
	if m.readOnly {
		return "accessor"
	}
	return "mutator"
}

// recordPrepare observes the cache-warming latency. It is a no-op when the cache
// has no metrics (e.g. a LocalCache built outside the factory in tests).
func (m *LocalCache) recordPrepare(start time.Time, err error) {
	if m.metrics == nil {
		return
	}
	status := "ok"
	if err != nil {
		status = "error"
	}
	m.metrics.prepareDuration.WithLabelValues(m.opLabel(), status).Observe(time.Since(start).Seconds())
}

// recordCommit counts a commit attempt by outcome. conflict reports whether the
// commit lost the manifest-pointer compare-and-swap; it is passed explicitly
// because Commit may rewrite the conflict into a caller-registered error that no
// longer matches ErrManifestPointerConflict. It is a no-op when the cache has no
// metrics.
func (m *LocalCache) recordCommit(err error, conflict bool) {
	if m.metrics == nil {
		return
	}
	status := "ok"
	switch {
	case conflict:
		status = "conflict"
	case err != nil:
		status = "error"
	}
	m.metrics.commitTotal.WithLabelValues(status).Inc()
}
