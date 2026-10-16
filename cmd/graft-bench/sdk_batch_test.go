package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/sdk"
)

// TestSDKBackendReportsBatchPlacement lands one change on a real server. A
// single land is alone in its batch, whether or not the server batches, so the
// placement is 1 of 1. The client is dialled with no option beyond storage and
// token: the placement comes from the SDK's land result, so any client reports
// it.
func TestSDKBackendReportsBatchPlacement(t *testing.T) {
	t.Parallel()

	addr, ctx, storage, relPath := startBenchServer(t)
	client, err := sdk.Dial(ctx, addr, sdk.WithStorage(storage), sdk.WithToken(benchToken))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	cfg := Config{Agents: 1, ChangesPerAgent: 1, Mode: ModeLand, Seed: 1, Branch: "bench-batch-placement"}
	res, err := Run(ctx, cfg, NewSDKBackend(client, relPath))
	require.NoError(t, err)
	require.Equal(t, 1, res.Landed, "errors: %v", res.Errors)
	require.Equal(t, map[int]int{1: 1}, res.BatchSizeHistogram)
	require.Equal(t, 1, res.BatchSizeMax)

	// The backend of any SDK client reports the placement of a single land.
	be := NewSDKBackend(client, relPath)
	base, err := be.Prepare(ctx, Config{Branch: "bench-batch-placement-direct"})
	require.NoError(t, err)
	c := Change{Agent: 1, Seq: 1, Target: "bench-batch-placement-direct"}
	commit, err := be.Commit(ctx, c, base)
	require.NoError(t, err)
	out, err := be.Land(ctx, Config{Mode: ModeLand}, c, commit)
	require.NoError(t, err)
	require.Equal(t, 1, out.BatchSize)
	require.Equal(t, 1, out.BatchPosition)
}

// TestSDKBackendConcurrentLandsReportBatches lands many changes on one branch
// at once. Whether they share batches depends on timing, so the test only
// checks what must hold for every batch: all changes land, and every batch
// size is at least one.
func TestSDKBackendConcurrentLandsReportBatches(t *testing.T) {
	t.Parallel()

	addr, ctx, storage, relPath := startBenchServer(t)
	client, err := sdk.Dial(ctx, addr, sdk.WithStorage(storage), sdk.WithToken(benchToken))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	cfg := Config{Agents: 30, ChangesPerAgent: 2, Mode: ModeLandBatch, Seed: 1, Branch: "bench-batch-concurrent"}
	res, err := Run(ctx, cfg, NewSDKBackend(client, relPath))
	require.NoError(t, err)
	require.Equal(t, 60, res.Landed, "errors: %v", res.Errors)
	require.Zero(t, res.Failed)

	var total int
	for size, changes := range res.BatchSizeHistogram {
		require.Positive(t, size)
		total += changes
	}
	require.Equal(t, res.Landed, total, "every landed change reports its batch")
	require.Positive(t, res.BatchSizeMax)
}
