package mvcc

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestLocalCache_recordCommit(t *testing.T) {
	m := newCacheMetrics()
	c := &LocalCache{metrics: m}

	c.recordCommit(nil, false)
	c.recordCommit(ErrManifestPointerConflict, true)
	// A caller-registered conflict error no longer matches ErrManifestPointerConflict,
	// so classification relies on the explicit conflict flag, not the error chain.
	c.recordCommit(errors.New("registered conflict error"), true)
	c.recordCommit(errors.New("boom"), false)

	require.Equal(t, float64(1), testutil.ToFloat64(m.commitTotal.WithLabelValues("ok")))
	require.Equal(t, float64(2), testutil.ToFloat64(m.commitTotal.WithLabelValues("conflict")))
	require.Equal(t, float64(1), testutil.ToFloat64(m.commitTotal.WithLabelValues("error")))
}

func TestLocalCache_recordPrepare(t *testing.T) {
	m := newCacheMetrics()

	accessor := &LocalCache{metrics: m, readOnly: true}
	mutator := &LocalCache{metrics: m, readOnly: false}

	accessor.recordPrepare(time.Now(), nil)
	mutator.recordPrepare(time.Now(), errors.New("boom"))

	// One series per (op, status) combination observed.
	require.Equal(t, 2, testutil.CollectAndCount(m.prepareDuration, "gitaly_mvcc_cache_prepare_duration_seconds"))
}

func TestLocalCache_recordMetricsNilSafe(t *testing.T) {
	c := &LocalCache{} // built outside the factory: no metrics

	require.NotPanics(t, func() {
		c.recordPrepare(time.Now(), nil)
		c.recordCommit(nil, false)
	})
}

func TestCacheFactory_isCollector(t *testing.T) {
	f := NewCacheFactory(t.TempDir(), nil)

	// A cache from the factory shares the factory's metrics, so recording through
	// it surfaces when collecting the factory itself.
	c := &LocalCache{metrics: f.cacheMetrics}
	c.recordCommit(nil, false)
	c.recordPrepare(time.Now(), nil)

	require.Equal(t, 2, testutil.CollectAndCount(f))
}
