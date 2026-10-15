package snapshot

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModeReadOnlyDirectory(t *testing.T) {
	require.Equal(t, "dr-x------", ModeReadOnlyDirectory.String())
}
