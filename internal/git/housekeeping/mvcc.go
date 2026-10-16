package housekeeping

import (
	"time"

	"github.com/feder-cr/git-ai/internal/helper/duration"
)

// MVCCConfig specifies MVCC-specific housekeeping configuration.
type MVCCConfig struct {
	// ManifestsToKeep controls how many manifests should be retained, starting from
	// the most recent.
	ManifestsToKeep uint
	// ManifestMinAge specifies how old a manifest must be (in terms of
	// its mtime) to be eligible for pruning.
	ManifestMinAge duration.Duration
	// ArtifactMinAge specifies how old an artifact no manifest references must be to be
	// eligible for removal.
	ArtifactMinAge duration.Duration
	// MaxLockDuration is the budget for what a run does while holding the repository lock.
	// Entries left over when it runs out are collected by the next run.
	MaxLockDuration duration.Duration
	// MaxAcquireWait is how long a run may wait for the repository lock before giving up.
	MaxAcquireWait duration.Duration
}

// DefaultMVCCConfig returns the default MVCC housekeeping configuration. We keep the 50 latest
// manifests, since it's unlikely that clients will be actively operating on a manifest older
// than that. Additionally, we only prune a manifest if it's at least 24 hours old, which is
// far longer than any RPC could be running.
//
// The lock budget only bounds a run that nothing asks to stop, as an arriving write evicts one.
// Ten seconds lets a sizeable backlog drain in a few runs while still bounding the hold when there
// is no write to evict it.
func DefaultMVCCConfig() MVCCConfig {
	return MVCCConfig{
		ManifestsToKeep: 50,
		ManifestMinAge:  duration.Duration(24 * time.Hour),
		ArtifactMinAge:  duration.Duration(24 * time.Hour),
		MaxLockDuration: duration.Duration(10 * time.Second),
		MaxAcquireWait:  duration.Duration(time.Second),
	}
}
