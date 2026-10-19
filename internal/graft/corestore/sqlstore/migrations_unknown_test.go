package sqlstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestOpenRefusesADatabaseOfANewerVersion verifies the startup refusal of a
// database that a newer version migrated (integrated review M1): Open fails
// with ErrUnknownMigrations, naming the migration and pointing at the upgrade,
// instead of the error of the migration library or a schema it cannot read.
func TestOpenRefusesADatabaseOfANewerVersion(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		const future = "29990101000000_from_a_newer_version"
		var appliedAt any = time.Now().UTC()
		if s.Dialect() == sqlstore.SQLite {
			appliedAt = time.Now().UTC().Format("2006-01-02 15:04:05")
		}
		execSQL(t, ctx, s, "INSERT INTO graft_storage_migrations (id, applied_at) VALUES (?, ?)", future, appliedAt)

		_, err := sqlstore.Open(ctx, coreDSN(t, s))
		require.ErrorIs(t, err, sqlstore.ErrUnknownMigrations)
		require.ErrorContains(t, err, future)
		require.ErrorContains(t, err, "upgrade this node")
	})
}
