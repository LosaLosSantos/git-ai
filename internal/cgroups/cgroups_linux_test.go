//go:build linux

package cgroups

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config/cgroups"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

func TestNewManagerNoop(t *testing.T) {
	require.IsType(t, &NoopManager{}, NewManager(cgroups.Config{}, testhelper.SharedLogger(t), 1))
}
