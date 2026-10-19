package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// GraftStorage configures the Graft storage core ([graft.storage]): the
// repository state in a strongly consistent database plus object storage,
// with the local git repositories as caches (doc/graft/STORAGE.md).
type GraftStorage struct {
	// Mode is "off" (the default, also when empty), "shadow" or
	// "authoritative".
	Mode string `json:"mode" toml:"mode,omitempty"`
	// Database is the database URL: "sqlite:///absolute/path.db" or
	// "postgres://user@host:port/db?sslmode=..." ("postgresql://" too).
	// Credentials come from the PG* environment variables or .pgpass.
	Database string `json:"database" toml:"database,omitempty"`
	// Bucket is the gocloud URL of the object storage: file:///absolute/dir,
	// s3://bucket?region=..., gs://bucket, azblob://container (mem:// for
	// tests). Credentials come from the cloud SDK environment.
	Bucket string `json:"bucket" toml:"bucket,omitempty"`
	// NodeID names this node in the database. Empty means the host name.
	NodeID string `json:"node_id" toml:"node_id,omitempty"`
	// SyncTimeout bounds the catch-up of a repository before a request is
	// served in authoritative mode (Go duration). Empty means
	// DefaultGraftStorageSyncTimeout.
	SyncTimeout string `json:"sync_timeout" toml:"sync_timeout,omitempty"`
	// ShadowTimeout bounds the shadow-mode work of one reference transaction
	// (Go duration). Empty means DefaultGraftStorageShadowTimeout.
	ShadowTimeout string `json:"shadow_timeout" toml:"shadow_timeout,omitempty"`
	// GCGrace is how long garbage collection waits before it deletes a pack it
	// observed unreferenced, and how old a checkpoint must be before the packs it
	// covers are deleted (Go duration). Empty means DefaultGraftStorageGCGrace.
	GCGrace string `json:"gc_grace" toml:"gc_grace,omitempty"`
	// GCMargin is the safety margin of the writers' deadline: a writer uploads a
	// pack and commits its changeset within gc_grace - gc_margin (Go duration).
	// Empty means DefaultGraftStorageGCMargin. See doc/graft/STORAGE.md for the
	// clock assumption it covers.
	GCMargin string `json:"gc_margin" toml:"gc_margin,omitempty"`
	// Checkpoints is "auto" or "off": the background checkpoints of the storage
	// core (doc/graft/PLAN7.md, "Background maintenance"). Empty means "auto" in
	// authoritative mode, where checkpoints exist, and "off" otherwise. A
	// checkpoint deletes nothing (it adds a pack and rows), and without one a
	// fresh node replays every changeset of a repository.
	Checkpoints string `json:"checkpoints" toml:"checkpoints,omitempty"`
	// GC is "off" (the default, also when empty) or "auto": the background
	// garbage collection, which deletes packs of the bucket. Opt-in, since it is
	// the one operation that destroys data. "auto" requires authoritative mode.
	// The graft storage commands work in either case.
	GC string `json:"gc" toml:"gc,omitempty"`
	// MaintenanceInterval is how often the background checkpoints and garbage
	// collection run (Go duration). Empty means
	// DefaultGraftStorageMaintenanceInterval.
	MaintenanceInterval string `json:"maintenance_interval" toml:"maintenance_interval,omitempty"`
	// CheckpointChangesets makes the maintenance checkpoint a repository once
	// this many changesets follow its newest checkpoint; 0 never checkpoints by
	// count. Unset means DefaultGraftStorageCheckpointChangesets.
	CheckpointChangesets *uint64 `json:"checkpoint_changesets" toml:"checkpoint_changesets,omitempty"`
	// CheckpointPackBytes makes the maintenance checkpoint a repository once the
	// packs of the changesets after its newest checkpoint hold this many bytes;
	// 0 never checkpoints by size. Unset means
	// DefaultGraftStorageCheckpointPackBytes.
	CheckpointPackBytes *int64 `json:"checkpoint_pack_bytes" toml:"checkpoint_pack_bytes,omitempty"`
	// LeaseTTL is how long a node holds the maintenance lease of a repository
	// before it must renew it (Go duration). Empty means
	// DefaultGraftStorageLeaseTTL.
	LeaseTTL string `json:"lease_ttl" toml:"lease_ttl,omitempty"`
}

// Values of GraftStorage.Checkpoints and GraftStorage.GC.
const (
	GraftStorageMaintenanceOff  = "off"
	GraftStorageMaintenanceAuto = "auto"
)

const (
	// DefaultGraftStorageMaintenanceInterval is the default
	// GraftStorage.MaintenanceInterval.
	DefaultGraftStorageMaintenanceInterval = time.Hour
	// DefaultGraftStorageCheckpointChangesets is the default
	// GraftStorage.CheckpointChangesets.
	DefaultGraftStorageCheckpointChangesets uint64 = 1000
	// DefaultGraftStorageCheckpointPackBytes is the default
	// GraftStorage.CheckpointPackBytes (256 MiB).
	DefaultGraftStorageCheckpointPackBytes int64 = 256 << 20
	// DefaultGraftStorageLeaseTTL is the default GraftStorage.LeaseTTL.
	DefaultGraftStorageLeaseTTL = 10 * time.Minute
)

// CheckpointsAuto reports whether the background checkpoints run: "auto", or
// unset in authoritative mode.
func (s GraftStorage) CheckpointsAuto() bool {
	switch s.Checkpoints {
	case GraftStorageMaintenanceAuto:
		return true
	case "":
		return s.Mode == "authoritative"
	}
	return false
}

// GCAuto reports whether the background garbage collection runs.
func (s GraftStorage) GCAuto() bool { return s.GC == GraftStorageMaintenanceAuto }

// MaintenanceAuto reports whether the background maintenance runs: its
// checkpoints, its garbage collection, or both.
func (s GraftStorage) MaintenanceAuto() bool { return s.CheckpointsAuto() || s.GCAuto() }

// MaintenanceIntervalOrDefault parses MaintenanceInterval, applying the default.
func (s GraftStorage) MaintenanceIntervalOrDefault() (time.Duration, error) {
	return positiveDurationOrDefault("maintenance_interval", s.MaintenanceInterval, DefaultGraftStorageMaintenanceInterval)
}

// LeaseTTLOrDefault parses LeaseTTL, applying the default.
func (s GraftStorage) LeaseTTLOrDefault() (time.Duration, error) {
	return positiveDurationOrDefault("lease_ttl", s.LeaseTTL, DefaultGraftStorageLeaseTTL)
}

// CheckpointChangesetsOrDefault returns CheckpointChangesets, applying the
// default.
func (s GraftStorage) CheckpointChangesetsOrDefault() uint64 {
	if s.CheckpointChangesets == nil {
		return DefaultGraftStorageCheckpointChangesets
	}
	return *s.CheckpointChangesets
}

// CheckpointPackBytesOrDefault returns CheckpointPackBytes, applying the
// default.
func (s GraftStorage) CheckpointPackBytesOrDefault() int64 {
	if s.CheckpointPackBytes == nil {
		return DefaultGraftStorageCheckpointPackBytes
	}
	return *s.CheckpointPackBytes
}

// validateMaintenance checks the background maintenance settings.
func (s GraftStorage) validateMaintenance() error {
	for _, setting := range []struct{ name, value string }{{"checkpoints", s.Checkpoints}, {"gc", s.GC}} {
		switch setting.value {
		case "", GraftStorageMaintenanceOff:
		case GraftStorageMaintenanceAuto:
			if s.Mode != "authoritative" {
				return fmt.Errorf("graft storage: %s = %q requires mode = \"authoritative\" (the background maintenance runs in authoritative mode only)", setting.name, setting.value)
			}
		default:
			return fmt.Errorf("graft storage: unknown %s %q (want off or auto)", setting.name, setting.value)
		}
	}

	if _, err := s.MaintenanceIntervalOrDefault(); err != nil {
		return err
	}
	if _, err := s.LeaseTTLOrDefault(); err != nil {
		return err
	}
	if s.CheckpointPackBytes != nil && *s.CheckpointPackBytes < 0 {
		return fmt.Errorf("graft storage: checkpoint_pack_bytes %d must not be negative", *s.CheckpointPackBytes)
	}
	return nil
}

const (
	// DefaultGraftStorageSyncTimeout is the default GraftStorage.SyncTimeout.
	DefaultGraftStorageSyncTimeout = 10 * time.Second
	// DefaultGraftStorageShadowTimeout is the default GraftStorage.ShadowTimeout.
	DefaultGraftStorageShadowTimeout = 5 * time.Second
	// DefaultGraftStorageGCGrace is the default GraftStorage.GCGrace.
	DefaultGraftStorageGCGrace = time.Hour
	// DefaultGraftStorageGCMargin is the default GraftStorage.GCMargin.
	DefaultGraftStorageGCMargin = 5 * time.Minute
)

// Enabled reports whether the storage core is on.
func (s GraftStorage) Enabled() bool { return s.Mode != "" && s.Mode != "off" }

// SyncTimeoutOrDefault parses SyncTimeout, applying the default.
func (s GraftStorage) SyncTimeoutOrDefault() (time.Duration, error) {
	return positiveDurationOrDefault("sync_timeout", s.SyncTimeout, DefaultGraftStorageSyncTimeout)
}

// ShadowTimeoutOrDefault parses ShadowTimeout, applying the default.
func (s GraftStorage) ShadowTimeoutOrDefault() (time.Duration, error) {
	return positiveDurationOrDefault("shadow_timeout", s.ShadowTimeout, DefaultGraftStorageShadowTimeout)
}

// GCGraceOrDefault parses GCGrace, applying the default.
func (s GraftStorage) GCGraceOrDefault() (time.Duration, error) {
	return positiveDurationOrDefault("gc_grace", s.GCGrace, DefaultGraftStorageGCGrace)
}

// GCMarginOrDefault parses GCMargin, applying the default.
func (s GraftStorage) GCMarginOrDefault() (time.Duration, error) {
	return positiveDurationOrDefault("gc_margin", s.GCMargin, DefaultGraftStorageGCMargin)
}

// validateGC checks that the garbage collection times leave the writers a
// positive deadline, and that a sync can never meet a pack the collector
// deletes during it: gc_grace > sync_timeout + gc_margin
// (doc/graft/PLAN7.md, GC BOUNDARY rule).
func (s GraftStorage) validateGC() error {
	grace, err := s.GCGraceOrDefault()
	if err != nil {
		return err
	}
	margin, err := s.GCMarginOrDefault()
	if err != nil {
		return err
	}
	sync, err := s.SyncTimeoutOrDefault()
	if err != nil {
		return err
	}

	if grace <= sync+margin {
		return fmt.Errorf("graft storage: gc_grace %s must be greater than sync_timeout %s + gc_margin %s", grace, sync, margin)
	}
	return nil
}

func positiveDurationOrDefault(name, value string, def time.Duration) (time.Duration, error) {
	if value == "" {
		return def, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("graft storage: invalid %s %q (want a positive Go duration)", name, value)
	}
	return d, nil
}

// validate checks the storage configuration. standalone is Graft.Standalone:
// authoritative mode requires it (Praefect is not supported in that mode).
func (s GraftStorage) validate(standalone bool) error {
	switch s.Mode {
	case "", "off":
		// The other settings are not read when the storage core is off, but a
		// maintenance that cannot run is an error, not silence.
		return s.validateMaintenance()
	case "shadow", "authoritative":
	default:
		return fmt.Errorf("graft storage: unknown mode %q (want off, shadow or authoritative)", s.Mode)
	}

	if s.Mode == "authoritative" && !standalone {
		return fmt.Errorf("graft storage: authoritative mode requires graft.standalone = true")
	}

	switch {
	case strings.HasPrefix(s.Database, "postgres://"), strings.HasPrefix(s.Database, "postgresql://"):
	case strings.HasPrefix(s.Database, "sqlite:///"):
	case s.Database == "":
		return fmt.Errorf("graft storage: database is required when the storage core is on")
	default:
		return fmt.Errorf("graft storage: unsupported database URL (want sqlite:///absolute/path or postgres://...)")
	}

	if s.Bucket == "" {
		return fmt.Errorf("graft storage: bucket is required when the storage core is on")
	}
	u, err := url.Parse(s.Bucket)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("graft storage: bucket must be a URL such as file:///dir or s3://bucket")
	}

	if strings.ContainsAny(s.NodeID, " \t\n/") {
		return fmt.Errorf("graft storage: node_id %q must not contain spaces or slashes", s.NodeID)
	}

	if _, err := s.SyncTimeoutOrDefault(); err != nil {
		return err
	}
	if _, err := s.ShadowTimeoutOrDefault(); err != nil {
		return err
	}
	if err := s.validateMaintenance(); err != nil {
		return err
	}

	return s.validateGC()
}
