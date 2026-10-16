package loadmonitor

import (
	"time"

	"github.com/feder-cr/git-ai/internal/cgroups"
)

// Stats wraps statistics from different sources.
type Stats struct {
	pollTime time.Time
	CGroup   cgroups.Stats
}

// PollTime returns the poll time of the current Stats instance
func (s Stats) PollTime() time.Time {
	return s.pollTime
}
