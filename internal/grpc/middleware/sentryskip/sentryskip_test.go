package sentryskip

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

func TestMarkToSkip(t *testing.T) {
	t.Parallel()

	t.Run("marked context is skipped", func(t *testing.T) {
		ctx := WithMarker(testhelper.Context(t))
		require.False(t, ShouldSkip(ctx))

		MarkToSkip(ctx)
		require.True(t, ShouldSkip(ctx))
	})

	t.Run("context without marker is a no-op", func(t *testing.T) {
		ctx := testhelper.Context(t)
		MarkToSkip(ctx)
		require.False(t, ShouldSkip(ctx))
	})
}
