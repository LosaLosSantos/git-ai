package storagemgr

import (
	"context"
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/keyvalue"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/keyvalue/databasemgr"
	"github.com/feder-cr/open_scion/internal/helper"
	logging "github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

func getTestDBManager(t *testing.T, ctx context.Context, cfg config.Cfg, logger logging.Logger) (*databasemgr.DBManager, keyvalue.Store) {
	t.Helper()

	dbMgr, err := databasemgr.NewDBManager(ctx, cfg.Storages, keyvalue.NewBadgerStore, helper.NewNullTickerFactory(), logger)
	require.NoError(t, err)
	t.Cleanup(dbMgr.Close)

	db, err := dbMgr.GetDB(cfg.Storages[0].Name)
	require.NoError(t, err)

	return dbMgr, db
}
