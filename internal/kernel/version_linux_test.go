package kernel

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsAtleast(t *testing.T) {
	// Go 1.17 requires kernel version of at least 2.6.32 so we use that as the assertion here.
	isAtLeast, err := IsAtLeast(Version{Major: 2, Minor: 6})
	require.NoError(t, err)
	require.True(t, isAtLeast)
}
