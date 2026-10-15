package praefect

import (
	"fmt"
	"testing"

	"github.com/feder-cr/open_scion/internal/praefect/config"
	"github.com/feder-cr/open_scion/internal/praefect/datastore/migrations"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestSubCmdSqlMigrate(t *testing.T) {
	db := testdb.New(t)
	dbCfg := testdb.GetConfig(t, db.Name)
	cfg := config.Config{
		ListenAddr:      "/dev/null",
		VirtualStorages: []*config.VirtualStorage{{Name: "p", Nodes: []*config.Node{{Storage: "s", Address: "localhost"}}}},
		DB:              dbCfg,
	}
	confPath := writeConfigToFile(t, cfg)

	migrationCt := len(migrations.All())

	for _, tc := range []struct {
		desc           string
		up             int
		args           []string
		expectedOutput []string
		expectedErr    error
	}{
		{
			desc:        "unexpected positional arguments",
			args:        []string{"positional-arg"},
			expectedErr: cli.Exit(unexpectedPositionalArgsError{Command: "sql-migrate"}, 1),
		},
		{
			desc:           "All migrations up",
			up:             migrationCt,
			expectedOutput: []string{"praefect sql-migrate: all migrations are up"},
		},
		{
			desc: "All migrations down",
			up:   0,
			expectedOutput: []string{
				fmt.Sprintf("praefect sql-migrate: migrations to apply: %d", migrationCt),
				"20200109161404_hello_world: migrating",
				"20200109161404_hello_world: applied (",
				fmt.Sprintf("praefect sql-migrate: OK (applied %d migrations)", migrationCt),
			},
		},
		{
			desc: "Some migrations down",
			up:   10,
			expectedOutput: []string{
				fmt.Sprintf("praefect sql-migrate: migrations to apply: %d", migrationCt-10),
				"20201126165633_repository_assignments_table: migrating",
				"20201126165633_repository_assignments_table: applied (",
				fmt.Sprintf("praefect sql-migrate: OK (applied %d migrations)", migrationCt-10),
			},
		},
		{
			desc: "Verbose output",
			up:   0,
			args: []string{"-verbose"},
			expectedOutput: []string{
				fmt.Sprintf("praefect sql-migrate: migrations to apply: %d", migrationCt),
				"20200109161404_hello_world: migrating",
				"[CREATE TABLE hello_world (id integer)]",
				"20200109161404_hello_world: applied (",
				fmt.Sprintf("praefect sql-migrate: OK (applied %d migrations)", migrationCt),
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := testhelper.Context(t)

			testdb.SetMigrations(t, db, cfg, tc.up)
			stdout, stderr, exitCode := runApp(t, ctx, append([]string{"-config", confPath, sqlMigrateCmdName, "-ignore-unknown"}, tc.args...))
			if tc.expectedErr != nil {
				require.Equal(t, tc.expectedErr.Error()+"\n", stderr)
				require.Equal(t, 1, exitCode)
				return
			}

			for _, out := range tc.expectedOutput {
				assert.Contains(t, stdout, out)
			}
			require.Empty(t, stderr)
			require.Zero(t, exitCode)
		})
	}
}

func TestSubCmdSqlMigrate_planFailure(t *testing.T) {
	db := testdb.New(t)
	dbCfg := testdb.GetConfig(t, db.Name)
	cfg := config.Config{
		ListenAddr:      "/dev/null",
		VirtualStorages: []*config.VirtualStorage{{Name: "p", Nodes: []*config.Node{{Storage: "s", Address: "localhost"}}}},
		DB:              dbCfg,
	}
	confPath := writeConfigToFile(t, cfg)
	ctx := testhelper.Context(t)

	// A recorded migration the binary does not know about makes planning fail
	// once -ignore-unknown, which defaults to true, is switched off. Before the
	// fix this printed "all migrations are up" and exited 0.
	testdb.SetMigrations(t, db, cfg, len(migrations.All()))
	_, err := db.ExecContext(ctx,
		"INSERT INTO "+migrations.MigrationTableName+" (id, applied_at) VALUES ('99999999999999_from_a_newer_praefect', NOW())",
	)
	require.NoError(t, err)

	stdout, stderr, exitCode := runApp(t, ctx, []string{"-config", confPath, sqlMigrateCmdName, "-ignore-unknown=false"})

	require.Equal(t, 1, exitCode)
	require.NotContains(t, stdout, "all migrations are up")
	require.Contains(t, stderr, "praefect sql-migrate: plan migrations:")
}
