//go:build !linux

package cgroups

import (
	"github.com/feder-cr/git-ai/internal/gitaly/config/cgroups"
	"github.com/feder-cr/git-ai/internal/log"
)

func newCgroupManager(cgroups.Config, log.Logger, int) Manager {
	return &NoopManager{}
}

// No-op. The actual pruning operations are implemented in Cgroup V1/V2 managers.
func pruneOldCgroups(cgroups.Config, log.Logger) {}
