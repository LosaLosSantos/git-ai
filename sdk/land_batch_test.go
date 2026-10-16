package sdk

import (
	"testing"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestBatchOf(t *testing.T) {
	t.Parallel()

	t.Run("nil response is not batched", func(t *testing.T) {
		t.Parallel()

		info := batchOf(nil)
		require.Equal(t, BatchInfo{}, info)
		require.False(t, info.Batched())
	})

	t.Run("batch fields are mapped", func(t *testing.T) {
		t.Parallel()

		info := batchOf(&gitalypb.UserLandResponse{BatchSize: 12, BatchPosition: 3})
		require.Equal(t, BatchInfo{Size: 12, Position: 3}, info)
		require.True(t, info.Batched())
	})

	t.Run("a change that landed alone is a batch of one", func(t *testing.T) {
		t.Parallel()

		info := batchOf(&gitalypb.UserLandResponse{BatchSize: 1, BatchPosition: 1})
		require.Equal(t, BatchInfo{Size: 1, Position: 1}, info)
		require.False(t, info.Batched())
	})
}
