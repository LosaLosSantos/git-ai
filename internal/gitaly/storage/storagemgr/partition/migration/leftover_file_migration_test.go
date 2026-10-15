package migration_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/mode"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/storagemgr/partition/migration"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestReportLostFoundDirectoryExistence(t *testing.T) {
	type testSetup func(*testing.T, string)
	noContentFn := func(t *testing.T, path string) {}
	hasContentFn := func(t *testing.T, path string) {
		require.NoError(t, os.MkdirAll(filepath.Join(path, migration.LostFoundPrefix), mode.Directory))
	}

	for _, tc := range []struct {
		desc             string
		storages         map[string]testSetup
		expectedStorages []string
	}{
		{
			desc: "storages have no garbage",
			storages: map[string]testSetup{
				"s1": noContentFn,
				"s2": noContentFn,
			},
		},
		{
			desc: "one storage has no garbage, the other has",
			storages: map[string]testSetup{
				"s1": noContentFn,
				"s2": hasContentFn,
			},
			expectedStorages: []string{"s2"},
		},
		{
			desc: "all storages have garbage",
			storages: map[string]testSetup{
				"s1": hasContentFn,
				"s2": hasContentFn,
				"s3": hasContentFn,
			},
			expectedStorages: []string{"s1", "s2", "s3"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			tmpDir := testhelper.TempDir(t)
			logger := testhelper.NewLogger(t)
			hook := testhelper.AddLoggerHook(logger)
			defer hook.Reset()

			cfg := testcfg.Build(t)
			cfg.Storages = make([]config.Storage, len(tc.storages))
			var idx int
			for storageName, setupFn := range tc.storages {
				cfg.Storages[idx].Name = storageName
				cfg.Storages[idx].Path = filepath.Join(tmpDir, storageName)
				idx++
				setupFn(t, filepath.Join(tmpDir, storageName))
			}

			migration.ReportLostFoundDirectoryExistence(logger, cfg)
			logEntries := hook.AllEntries()
			require.Equal(t, len(tc.expectedStorages), len(logEntries))
			actualStorage := make([]string, 0, len(tc.storages))
			if len(tc.expectedStorages) > 0 {
				for _, entry := range logEntries {
					require.Contains(t, entry.Message, "leftover migration garbage dir is detected")
					actualStorage = append(actualStorage, entry.Data["storageName"].(string))
				}
				require.ElementsMatch(t, tc.expectedStorages, actualStorage)
			}
		})
	}
}
