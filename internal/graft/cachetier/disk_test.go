package cachetier

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// diskBytes returns the total size of the files below root.
func diskBytes(t *testing.T, root string) int64 {
	t.Helper()

	var total int64
	require.NoError(t, filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	}))
	return total
}

// TestDiskStore_StaysWithinBudget checks that the on-disk store evicts the oldest
// objects once it is over budget, instead of growing without limit.
func TestDiskStore_StaysWithinBudget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	const budget = 1000
	disk, err := newDiskStore(root, budget)
	require.NoError(t, err)

	repo := testRepo("@hashed/disk.git")
	data := make([]byte, 100)
	var oids []string
	for i := 0; i < 30; i++ {
		data[0] = byte(i)
		oid := gitObjectID("blob", data, false)
		oids = append(oids, oid)

		key := objectKey{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(), oid: oid}
		require.NoError(t, disk.write(key, &object{typ: gitalypb.ObjectType_BLOB, data: append([]byte(nil), data...)}))

		// File timestamps are coarse, so set the write age explicitly. Object i is
		// older than object i+1.
		written := time.Unix(1_700_000_000+int64(i), 0)
		require.NoError(t, os.Chtimes(disk.path(key), written, written))

		require.LessOrEqual(t, diskBytes(t, root), int64(budget), "after write %d", i)
	}

	// The newest object is never the eviction victim before the older ones.
	last := objectKey{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(), oid: oids[len(oids)-1]}
	obj, err := disk.read(last, noReadLimit)
	require.NoError(t, err)
	require.NotNil(t, obj, "the most recent object must survive")

	first := objectKey{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(), oid: oids[0]}
	obj, err = disk.read(first, noReadLimit)
	require.NoError(t, err)
	require.Nil(t, obj, "the oldest object must have been evicted")

	_, err = os.Stat(disk.path(first))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestDiskStore_ReopenAccountsForExistingFiles checks that a store reopened over a
// populated directory counts the existing files against its budget.
func TestDiskStore_ReopenAccountsForExistingFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	const budget = 1000
	disk, err := newDiskStore(root, budget)
	require.NoError(t, err)

	repo := testRepo("@hashed/reopen.git")
	data := make([]byte, 100)
	for i := 0; i < 5; i++ {
		data[0] = byte(i)
		oid := gitObjectID("blob", data, false)
		key := objectKey{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(), oid: oid}
		require.NoError(t, disk.write(key, &object{typ: gitalypb.ObjectType_BLOB, data: append([]byte(nil), data...)}))
	}

	reopened, err := newDiskStore(root, budget)
	require.NoError(t, err)
	require.Equal(t, diskBytes(t, root), reopened.used)
}
