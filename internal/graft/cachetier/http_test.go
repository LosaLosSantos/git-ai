package cachetier

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestHTTPHandler_StatsAndMetrics(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/http.git")
	upstream := newFakeObjectService()
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("hello"), false)

	tier := newTier(t, upstream, Config{MaxBytes: 1 << 20})
	req := &gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}}

	// The first read misses and fills the cache, the second is a hit.
	getObjects(t, ctx, tier.client, req)
	getObjects(t, ctx, tier.client, req)

	handler := tier.server.HTTPHandler()

	t.Run("stats", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var stats Stats
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &stats))
		require.Equal(t, TierCache, stats.Tier)
		require.Equal(t, int64(1<<20), stats.MaxBytes)
		require.Equal(t, int64(1), stats.ObjectMisses)
		require.Equal(t, int64(1), stats.ObjectHits)
		require.Equal(t, 1, stats.MemoryEntries)
		require.Positive(t, stats.MemoryBytes)
		require.Zero(t, stats.Evictions)
		require.Zero(t, stats.VerifyFailures)
	})

	t.Run("metrics", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

		require.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		require.True(t, strings.Contains(body, "\ngraft_cache_object_hits_total 1\n"), body)
		require.True(t, strings.Contains(body, "\ngraft_cache_object_misses_total 1\n"), body)
		require.True(t, strings.Contains(body, "\ngraft_cache_entries 1\n"), body)
	})
}
