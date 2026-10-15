package landlane

import (
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// gatherValues returns the value of every gauge and the sample count of every
// histogram of reg, by metric name.
func gatherValues(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)

	values := map[string]float64{}
	for _, family := range families {
		for _, m := range family.GetMetric() {
			switch {
			case m.GetGauge() != nil:
				values[family.GetName()] = m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				values[family.GetName()] = float64(m.GetHistogram().GetSampleCount())
			}
		}
	}

	return values
}

func TestPrometheusObserverExportsMetrics(t *testing.T) {
	ctx := testhelper.Context(t)

	obs := NewPrometheusObserver()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(obs))

	gate := make(chan struct{})
	entered := make(chan struct{}, 10)
	rec := &recorder{}
	r := newRegistry(t, unbatched(50), echoExecutor(rec, gate, entered), obs)

	first := submitAsync(ctx, r, testKey, "r0")
	<-entered

	second := submitAsync(ctx, r, testKey, "r1")
	require.Eventually(t, func() bool { return queueLen(r) == 1 }, 10*time.Second, time.Millisecond)

	require.Equal(t, 1.0, gatherValues(t, reg)["graft_land_lane_queue_depth"], "queued request must be visible")

	close(gate)
	require.NoError(t, requireResult(t, first).err)
	require.NoError(t, requireResult(t, second).err)

	values := gatherValues(t, reg)
	require.Equal(t, 0.0, values["graft_land_lane_queue_depth"], "idle lanes must not queue anything")
	require.Equal(t, 2.0, values["graft_land_batch_size"], "one observation per batch")
	require.Equal(t, 2.0, values["graft_land_lane_wait_seconds"], "one observation per request")
}

func TestPrometheusObserverSumsQueueDepthOverLanes(t *testing.T) {
	obs := NewPrometheusObserver()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(obs))

	obs.QueueDepth(Key{Branch: "a"}, 3)
	obs.QueueDepth(Key{Branch: "b"}, 2)
	require.Equal(t, 5.0, gatherValues(t, reg)["graft_land_lane_queue_depth"])

	obs.QueueDepth(Key{Branch: "a"}, 0)
	require.Equal(t, 2.0, gatherValues(t, reg)["graft_land_lane_queue_depth"])
}

func TestPrometheusObserverMetricNames(t *testing.T) {
	obs := NewPrometheusObserver()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(obs))

	obs.BatchExecuted(testKey, 4, time.Millisecond)
	obs.RequestWaited(testKey, 2*time.Second)

	values := gatherValues(t, reg)
	require.Contains(t, values, "graft_land_lane_queue_depth")
	require.Equal(t, 1.0, values["graft_land_batch_size"])
	require.Equal(t, 1.0, values["graft_land_lane_wait_seconds"])
}

func TestPrometheusObserverZeroValueIsUsable(t *testing.T) {
	var obs PrometheusObserver

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(&obs))

	require.NotPanics(t, func() {
		obs.QueueDepth(testKey, 3)
		obs.BatchExecuted(testKey, 3, time.Millisecond)
		obs.RequestWaited(testKey, time.Millisecond)
	})

	values := gatherValues(t, reg)
	require.Equal(t, 3.0, values["graft_land_lane_queue_depth"])
	require.Equal(t, 1.0, values["graft_land_batch_size"])
	require.Equal(t, 1.0, values["graft_land_lane_wait_seconds"])
}

func TestPrometheusObserverRegistersOnce(t *testing.T) {
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(NewPrometheusObserver()))
	require.Error(t, reg.Register(NewPrometheusObserver()), "the same metric names must not register twice")
}
