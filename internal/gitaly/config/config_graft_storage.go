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
}

const (
	// DefaultGraftStorageSyncTimeout is the default GraftStorage.SyncTimeout.
	DefaultGraftStorageSyncTimeout = 10 * time.Second
	// DefaultGraftStorageShadowTimeout is the default GraftStorage.ShadowTimeout.
	DefaultGraftStorageShadowTimeout = 5 * time.Second
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
		return nil
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

	return nil
}
