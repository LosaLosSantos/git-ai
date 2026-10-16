package storage

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

func TestNeedsSync(t *testing.T) {
	ctx := testhelper.Context(t)

	type mockTransaction struct{ Transaction }

	require.True(t, NeedsSync(ctx))
	require.False(t, NeedsSync(ContextWithTransaction(ctx, &mockTransaction{})))
}
