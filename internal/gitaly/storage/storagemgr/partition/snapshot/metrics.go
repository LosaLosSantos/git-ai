package snapshot

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics contains the global counters that haven't yet
// been scoped for a specific Manager.
type Metrics struct {
	createdExclusiveSnapshotTotal   *prometheus.CounterVec
	destroyedExclusiveSnapshotTotal *prometheus.CounterVec
	createdSharedSnapshotTotal      *prometheus.CounterVec
	reusedSharedSnapshotTotal       *prometheus.CounterVec
	destroyedSharedSnapshotTotal    *prometheus.CounterVec
	snapshotCreationDuration        *prometheus.HistogramVec
	snapshotDirectoryEntries        *prometheus.HistogramVec
}

// Describe implements prometheus.Collector.
func (m Metrics) Describe(descs chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(m, descs)
}

// Collect implements prometheus.Collector.
func (m Metrics) Collect(metrics chan<- prometheus.Metric) {
	m.createdExclusiveSnapshotTotal.Collect(metrics)
	m.destroyedExclusiveSnapshotTotal.Collect(metrics)
	m.createdSharedSnapshotTotal.Collect(metrics)
	m.reusedSharedSnapshotTotal.Collect(metrics)
	m.destroyedSharedSnapshotTotal.Collect(metrics)
	m.snapshotCreationDuration.Collect(metrics)
	m.snapshotDirectoryEntries.Collect(metrics)
}

// NewMetrics returns a new Metrics instance.
func NewMetrics() Metrics {
	labels := []string{"storage"}
	return Metrics{
		createdExclusiveSnapshotTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gitaly_exclusive_snapshots_created_total",
			Help: "Number of created exclusive snapshots.",
		}, labels),
		destroyedExclusiveSnapshotTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gitaly_exclusive_snapshots_destroyed_total",
			Help: "Number of destroyed exclusive snapshots.",
		}, labels),
		createdSharedSnapshotTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gitaly_shared_snapshots_created_total",
			Help: "Number of created shared snapshots.",
		}, labels),
		reusedSharedSnapshotTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gitaly_shared_snapshots_reused_total",
			Help: "Number of reused shared snapshots.",
		}, labels),
		destroyedSharedSnapshotTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gitaly_shared_snapshots_destroyed_total",
			Help: "Number of destroyed shared snapshots.",
		}, labels),
		snapshotCreationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gitaly_snapshot_creation_duration_seconds",
			Help:    "Time spent creating snapshots.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
		}, labels),
		snapshotDirectoryEntries: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gitaly_snapshot_directory_entries",
			Help:    "Number directories and files in snapshots.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12),
		}, labels),
	}
}

// ManagerMetrics contains the metrics supported by the Manager.
type ManagerMetrics struct {
	createdExclusiveSnapshotTotal   prometheus.Counter
	destroyedExclusiveSnapshotTotal prometheus.Counter
	createdSharedSnapshotTotal      prometheus.Counter
	reusedSharedSnapshotTotal       prometheus.Counter
	destroyedSharedSnapshotTotal    prometheus.Counter
	snapshotCreationDuration        prometheus.Observer
	snapshotDirectoryEntries        prometheus.Observer
}

// Scope returns the metrics scoped for a given Manager.
func (m Metrics) Scope(storageName string) ManagerMetrics {
	labels := prometheus.Labels{"storage": storageName}
	return ManagerMetrics{
		createdExclusiveSnapshotTotal:   m.createdExclusiveSnapshotTotal.With(labels),
		destroyedExclusiveSnapshotTotal: m.destroyedExclusiveSnapshotTotal.With(labels),
		createdSharedSnapshotTotal:      m.createdSharedSnapshotTotal.With(labels),
		reusedSharedSnapshotTotal:       m.reusedSharedSnapshotTotal.With(labels),
		destroyedSharedSnapshotTotal:    m.destroyedSharedSnapshotTotal.With(labels),
		snapshotCreationDuration:        m.snapshotCreationDuration.With(labels),
		snapshotDirectoryEntries:        m.snapshotDirectoryEntries.With(labels),
	}
}
