package datastore

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/praefect/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testdb"
	"github.com/stretchr/testify/require"
)

func TestMigrateStatus(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	db := testdb.New(t)

	config := config.Config{
		DB: testdb.GetConfig(t, db.Name),
	}

	_, err := db.ExecContext(ctx, "INSERT INTO schema_migrations VALUES ('2020_01_01_test', NOW())")
	require.NoError(t, err)

	rows, err := MigrateStatus(config)
	require.NoError(t, err)

	m := rows["20200109161404_hello_world"]
	require.True(t, m.Migrated)
	require.False(t, m.Unknown)

	m = rows["2020_01_01_test"]
	require.True(t, m.Migrated)
	require.True(t, m.Unknown)
}
