package metrics

import (
	"fmt"

	gitalycfgprom "github.com/feder-cr/git-ai/internal/gitaly/config/prometheus"
	"github.com/feder-cr/git-ai/internal/praefect/datastore"
	"github.com/feder-cr/git-ai/internal/prometheus/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// RegisterReplicationDelay creates and registers a prometheus histogram
// to observe replication delay times
func RegisterReplicationDelay(conf gitalycfgprom.Config, registerer prometheus.Registerer) (metrics.HistogramVec, error) {
	replicationDelay := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "gitaly",
			Subsystem: "praefect",
			Name:      "replication_delay",
			Buckets:   conf.GRPCLatencyBuckets,
		},
		[]string{"type"},
	)

	for _, changeType := range datastore.GetAllChangeTypes() {
		// We don't care about the observer here, we only want to initialize the
		// metric with the various label values
		_, err := replicationDelay.GetMetricWithLabelValues(changeType.String())
		if err != nil {
			return replicationDelay, fmt.Errorf("adding default label value %s failed: %w", changeType, err)
		}
	}

	return replicationDelay, registerer.Register(replicationDelay)
}

// RegisterReplicationLatency creates and registers a prometheus histogram
// to observe replication latency times
func RegisterReplicationLatency(conf gitalycfgprom.Config, registerer prometheus.Registerer) (metrics.HistogramVec, error) {
	replicationLatency := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "gitaly",
			Subsystem: "praefect",
			Name:      "replication_latency",
			Buckets:   conf.GRPCLatencyBuckets,
		},
		[]string{"type"},
	)

	for _, changeType := range datastore.GetAllChangeTypes() {
		// We don't care about the observer here, we only want to initialize the
		// metric with the various label values
		_, err := replicationLatency.GetMetricWithLabelValues(changeType.String())
		if err != nil {
			return replicationLatency, fmt.Errorf("adding default label value %s failed: %w", changeType, err)
		}
	}

	return replicationLatency, registerer.Register(replicationLatency)
}

// RegisterNodeLatency creates and registers a prometheus histogram to
// observe internal node latency
func RegisterNodeLatency(conf gitalycfgprom.Config, registerer prometheus.Registerer) (metrics.HistogramVec, error) {
	nodeLatency := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "gitaly",
			Subsystem: "praefect",
			Name:      "node_latency",
			Buckets:   conf.GRPCLatencyBuckets,
		}, []string{"gitaly_storage"},
	)

	return nodeLatency, registerer.Register(nodeLatency)
}

//nolint:revive // This is unintentionally missing documentation.
var MethodTypeCounter = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "gitaly",
		Subsystem: "praefect",
		Name:      "method_types",
	}, []string{"method_type"},
)

//nolint:revive // This is unintentionally missing documentation.
var PrimaryGauge = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "gitaly",
		Subsystem: "praefect",
		Name:      "primaries",
	}, []string{"virtual_storage", "gitaly_storage"},
)

//nolint:revive // This is unintentionally missing documentation.
var NodeLastHealthcheckGauge = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "gitaly",
		Subsystem: "praefect",
		Name:      "node_last_healthcheck_up",
	}, []string{"gitaly_storage"},
)

// ReadDistribution counts how many read operations was routed to each storage.
var ReadDistribution = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "gitaly",
		Subsystem: "praefect",
		Name:      "read_distribution",
		Help:      "Counts read operations directed to the storages",
	},
	[]string{"virtual_storage", "storage"},
)
