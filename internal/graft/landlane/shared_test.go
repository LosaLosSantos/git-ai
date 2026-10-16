package landlane

import (
	"context"
	"testing"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestSharedRegistryIsBuiltOnce(t *testing.T) {
	ctx := testhelper.Context(t)

	shared := NewShared(unbatched(10))
	defer shared.Close()

	first := &recorder{}
	second := &recorder{}

	r1, err := SharedRegistry(shared, echoExecutor(first, nil, nil))
	require.NoError(t, err)
	r2, err := SharedRegistry(shared, echoExecutor(second, nil, nil))
	require.NoError(t, err)
	require.Same(t, r1, r2, "every caller must get the same registry")

	// Both callers share the lanes, so the executor of the first one runs the batches of both.
	resp, err := r2.Submit(ctx, testKey, "r0")
	require.NoError(t, err)
	require.Equal(t, "landed-r0", resp.Value)
	require.Equal(t, [][]string{{"r0"}}, first.snapshot())
	require.Empty(t, second.snapshot())
}

func TestSharedRegistryRejectsOtherTypes(t *testing.T) {
	shared := NewShared(unbatched(10))
	defer shared.Close()

	_, err := SharedRegistry(shared, echoExecutor(&recorder{}, nil, nil))
	require.NoError(t, err)

	_, err = SharedRegistry(shared, func(context.Context, Key, []int) ([]Result[int], error) {
		return nil, nil
	})
	require.ErrorContains(t, err, "shared land lanes hold a")
}

func TestSharedRegistryRejectsInvalidConfig(t *testing.T) {
	shared := NewShared(Config{Enabled: true, MaxSize: 0})
	defer shared.Close()

	_, err := SharedRegistry(shared, echoExecutor(&recorder{}, nil, nil))
	require.ErrorContains(t, err, "max_size must be at least 1")
}

func TestSharedCloseClosesRegistry(t *testing.T) {
	ctx := testhelper.Context(t)

	shared := NewShared(unbatched(10))

	r, err := SharedRegistry(shared, echoExecutor(&recorder{}, nil, nil))
	require.NoError(t, err)

	shared.Close()

	_, err = r.Submit(ctx, testKey, "r0")
	require.ErrorIs(t, err, ErrClosed)

	_, err = SharedRegistry(shared, echoExecutor(&recorder{}, nil, nil))
	require.ErrorIs(t, err, ErrClosed)

	// Closing twice, or closing lanes whose registry was never built, does nothing.
	shared.Close()
	NewShared(unbatched(10)).Close()
}

func TestSharedExportsMetricsOfItsRegistry(t *testing.T) {
	ctx := testhelper.Context(t)

	shared := NewShared(unbatched(10))
	defer shared.Close()

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(shared))

	r, err := SharedRegistry(shared, echoExecutor(&recorder{}, nil, nil))
	require.NoError(t, err)

	_, err = r.Submit(ctx, testKey, "r0")
	require.NoError(t, err)

	values := gatherValues(t, reg)
	require.Contains(t, values, "graft_land_lane_queue_depth")
	require.Equal(t, 1.0, values["graft_land_batch_size"])
	require.Equal(t, 1.0, values["graft_land_lane_wait_seconds"])
}
