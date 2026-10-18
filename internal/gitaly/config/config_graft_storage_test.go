package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGraftStorageValidate(t *testing.T) {
	t.Parallel()

	valid := GraftStorage{Mode: "shadow", Database: "sqlite:///var/opt/graft/meta.db", Bucket: "file:///var/opt/graft/objects"}

	for _, tc := range []struct {
		desc       string
		storage    GraftStorage
		standalone bool
		err        string
	}{
		{desc: "off by default", storage: GraftStorage{}},
		{desc: "explicitly off ignores the rest", storage: GraftStorage{Mode: "off", Database: "nonsense"}},
		{desc: "shadow", storage: valid},
		{desc: "shadow with postgres", storage: GraftStorage{Mode: "shadow", Database: "postgres://u@h/db", Bucket: "s3://b"}},
		{desc: "postgresql scheme", storage: GraftStorage{Mode: "shadow", Database: "postgresql://u@h/db", Bucket: "mem://"}},
		{desc: "authoritative in standalone mode", storage: GraftStorage{Mode: "authoritative", Database: valid.Database, Bucket: valid.Bucket}, standalone: true},
		{desc: "authoritative needs standalone", storage: GraftStorage{Mode: "authoritative", Database: valid.Database, Bucket: valid.Bucket}, err: "requires graft.standalone"},
		{desc: "unknown mode", storage: GraftStorage{Mode: "primary"}, err: "unknown mode"},
		{desc: "missing database", storage: GraftStorage{Mode: "shadow", Bucket: valid.Bucket}, err: "database is required"},
		{desc: "relative sqlite path", storage: GraftStorage{Mode: "shadow", Database: "sqlite://meta.db", Bucket: valid.Bucket}, err: "unsupported database URL"},
		{desc: "unknown database", storage: GraftStorage{Mode: "shadow", Database: "mysql://h/db", Bucket: valid.Bucket}, err: "unsupported database URL"},
		{desc: "missing bucket", storage: GraftStorage{Mode: "shadow", Database: valid.Database}, err: "bucket is required"},
		{desc: "bucket without scheme", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: "/var/opt/objects"}, err: "bucket must be a URL"},
		{desc: "node id with slash", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, NodeID: "a/b"}, err: "node_id"},
		{desc: "bad sync timeout", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, SyncTimeout: "0s"}, err: "sync_timeout"},
		{desc: "bad shadow timeout", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, ShadowTimeout: "soon"}, err: "shadow_timeout"},
		{desc: "gc times", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GCGrace: "30m", GCMargin: "1m"}},
		{desc: "bad gc_grace", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GCGrace: "-1h"}, err: "gc_grace"},
		{desc: "bad gc_margin", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GCMargin: "0"}, err: "gc_margin"},
		{desc: "gc_grace not above sync_timeout + gc_margin", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GCGrace: "6m", GCMargin: "5m", SyncTimeout: "1m"}, err: "gc_grace 6m0s must be greater than sync_timeout 1m0s + gc_margin 5m0s"},
		{desc: "gc_grace just above sync_timeout + gc_margin", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GCGrace: "361s", GCMargin: "5m", SyncTimeout: "1m"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			err := tc.storage.validate(tc.standalone)
			if tc.err == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.err)
		})
	}
}

func TestGraftStorageDefaults(t *testing.T) {
	t.Parallel()

	var s GraftStorage
	require.False(t, s.Enabled())

	sync, err := s.SyncTimeoutOrDefault()
	require.NoError(t, err)
	require.Equal(t, DefaultGraftStorageSyncTimeout, sync)

	shadow, err := s.ShadowTimeoutOrDefault()
	require.NoError(t, err)
	require.Equal(t, DefaultGraftStorageShadowTimeout, shadow)

	grace, err := s.GCGraceOrDefault()
	require.NoError(t, err)
	require.Equal(t, DefaultGraftStorageGCGrace, grace)

	margin, err := s.GCMarginOrDefault()
	require.NoError(t, err)
	require.Equal(t, DefaultGraftStorageGCMargin, margin)
	require.NoError(t, s.validateGC(), "the defaults are consistent")

	s = GraftStorage{Mode: "shadow", SyncTimeout: "250ms"}
	require.True(t, s.Enabled())
	sync, err = s.SyncTimeoutOrDefault()
	require.NoError(t, err)
	require.Equal(t, 250*time.Millisecond, sync)
}

func TestGraftValidateChecksStorage(t *testing.T) {
	t.Parallel()

	require.ErrorContains(t, Graft{Storage: GraftStorage{Mode: "authoritative", Database: "sqlite:///x.db", Bucket: "mem://"}}.Validate(), "requires graft.standalone")
	require.NoError(t, Graft{Standalone: true, Storage: GraftStorage{Mode: "authoritative", Database: "sqlite:///x.db", Bucket: "mem://"}}.Validate())
}
