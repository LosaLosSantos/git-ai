package landlane

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusObserver is an Observer that exports the batching metrics. It
// implements prometheus.Collector, register it with the Prometheus registry
// once:
//
//   - graft_land_lane_queue_depth (gauge): queued requests, summed over lanes.
//   - graft_land_lane_effective_max_size (gauge): effective max batch size,
//     summed over the keys that have a lane or whose last lane was retired
//     less than KeyStateTTL ago (the keys the Registry keeps a size for).
//   - graft_land_batch_size (histogram): requests per executed batch.
//   - graft_land_lane_wait_seconds (histogram): time from submission to the
//     start of the execution, per request.
//
// Use NewPrometheusObserver. The zero value is also usable: the metrics are
// created on first use.
type PrometheusObserver struct {
	setup sync.Once

	// mu guards depths and maxSizes. QueueDepth and MaxSizeChanged are called
	// with the registry lock held, so the observer must not take any registry
	// lock.
	mu       sync.Mutex
	depths   map[Key]int
	maxSizes map[Key]int

	queueDepth       prometheus.Collector
	effectiveMaxSize prometheus.Collector
	batchSize        prometheus.Histogram
	waitSeconds      prometheus.Histogram
}

// NewPrometheusObserver creates an observer with its metrics. The metrics are
// not registered until the observer is registered as a collector.
func NewPrometheusObserver() *PrometheusObserver {
	o := &PrometheusObserver{}
	o.initOnce()

	return o
}

// initOnce creates the metrics and the depth map. Every method calls it, so
// that the zero value of PrometheusObserver works.
func (o *PrometheusObserver) initOnce() {
	o.setup.Do(func() {
		o.depths = map[Key]int{}
		o.maxSizes = map[Key]int{}

		o.queueDepth = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "graft_land_lane_queue_depth",
			Help: "Number of land requests queued in all lanes.",
		}, o.totalDepth)

		o.effectiveMaxSize = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "graft_land_lane_effective_max_size",
			Help: "Effective maximum number of land requests per batch, summed over the lanes and the recently used keys.",
		}, o.totalMaxSize)

		o.batchSize = prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "graft_land_batch_size",
			Help:    "Number of land requests executed together in one batch.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 8),
		})

		o.waitSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "graft_land_lane_wait_seconds",
			Help:    "Time a land request waited in its lane before its execution started.",
			Buckets: prometheus.DefBuckets,
		})
	})
}

// BatchExecuted implements Observer.
func (o *PrometheusObserver) BatchExecuted(_ Key, size int, _ time.Duration) {
	o.initOnce()
	o.batchSize.Observe(float64(size))
}

// RequestWaited implements Observer.
func (o *PrometheusObserver) RequestWaited(_ Key, wait time.Duration) {
	o.initOnce()
	o.waitSeconds.Observe(wait.Seconds())
}

// QueueDepth implements Observer.
func (o *PrometheusObserver) QueueDepth(key Key, depth int) {
	o.initOnce()

	o.mu.Lock()
	defer o.mu.Unlock()

	if depth == 0 {
		delete(o.depths, key)
		return
	}

	o.depths[key] = depth
}

// Describe implements prometheus.Collector.
func (o *PrometheusObserver) Describe(ch chan<- *prometheus.Desc) {
	o.initOnce()

	o.queueDepth.Describe(ch)
	o.effectiveMaxSize.Describe(ch)
	o.batchSize.Describe(ch)
	o.waitSeconds.Describe(ch)
}

// Collect implements prometheus.Collector.
func (o *PrometheusObserver) Collect(ch chan<- prometheus.Metric) {
	o.initOnce()

	o.queueDepth.Collect(ch)
	o.effectiveMaxSize.Collect(ch)
	o.batchSize.Collect(ch)
	o.waitSeconds.Collect(ch)
}

// MaxSizeChanged implements Observer.
func (o *PrometheusObserver) MaxSizeChanged(key Key, size int) {
	o.initOnce()

	o.mu.Lock()
	defer o.mu.Unlock()

	if size == 0 {
		delete(o.maxSizes, key)
		return
	}

	o.maxSizes[key] = size
}

func (o *PrometheusObserver) totalDepth() float64 {
	o.mu.Lock()
	defer o.mu.Unlock()

	total := 0
	for _, depth := range o.depths {
		total += depth
	}

	return float64(total)
}

func (o *PrometheusObserver) totalMaxSize() float64 {
	o.mu.Lock()
	defer o.mu.Unlock()

	total := 0
	for _, size := range o.maxSizes {
		total += size
	}

	return float64(total)
}
