package changelog

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sequencesAndReferences lists the log of repoPath as sequence and reference pairs.
func sequencesAndReferences(t *testing.T, repoPath string) []string {
	t.Helper()

	entries, err := List(repoPath, 0, 100)
	require.NoError(t, err)
	var listed []string
	for _, entry := range entries {
		listed = append(listed, filepath.Base(entry.Reference)+"@"+string(rune('0'+entry.Sequence)))
	}
	return listed
}

// This test replaces syncDir, so it must not run in parallel with other tests. A reader whose
// process did not append syncs the directories of the log before it serves entries: the writer
// may have crashed between the fsync of the file and those of the directories.
func TestReaders_syncDirectoriesOfAnotherProcess(t *testing.T) {
	var (
		mu     sync.Mutex
		synced []string
	)
	originalSyncDir := syncDir
	t.Cleanup(func() { syncDir = originalSyncDir })
	syncDir = func(dir string) error {
		mu.Lock()
		synced = append(synced, dir)
		mu.Unlock()
		return originalSyncDir(dir)
	}
	takeSynced := func() []string {
		mu.Lock()
		defer mu.Unlock()
		result := synced
		synced = nil
		return result
	}

	repoPath := newRepoPath(t)
	_, err := Append(repoPath, Meta{Operation: "test"}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)
	takeSynced()

	// A reader of another process: this process's state is gone.
	dropState(repoPath)
	require.Equal(t, []string{"a@1"}, sequencesAndReferences(t, repoPath))
	require.ElementsMatch(t, []string{filepath.Dir(Path(repoPath)), repoPath}, takeSynced())

	// Once synced, the next read does not sync them again.
	require.Equal(t, []string{"a@1"}, sequencesAndReferences(t, repoPath))
	require.Empty(t, takeSynced())
}

// This test lowers lockWaitTimeout, so it must not run in parallel with other tests. A lock file
// that another process never releases fails the appends and the reads after lockWaitTimeout, and
// the log works again once it is released.
func TestAppend_lockWaitIsBounded(t *testing.T) {
	originalTimeout := lockWaitTimeout
	t.Cleanup(func() { lockWaitTimeout = originalTimeout })
	lockWaitTimeout = 100 * time.Millisecond

	repoPath := newRepoPath(t)
	_, err := Append(repoPath, Meta{Operation: "test"}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)

	// Another process holds the lock file.
	holder, err := os.OpenFile(Path(repoPath)+lockSuffix, os.O_RDWR, 0o644)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_EX))

	_, err = Append(repoPath, Meta{Operation: "test"}, []Update{{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: oidB}})
	require.ErrorContains(t, err, "the lock was not granted within 100ms")

	// A reader that has to wait for the lock (it comes without the state) fails the same way.
	dropState(repoPath)
	_, err = List(repoPath, 0, 10)
	require.ErrorContains(t, err, "the lock was not granted within 100ms")

	require.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_UN))
	require.NoError(t, holder.Close())

	_, err = Append(repoPath, Meta{Operation: "test"}, []Update{{Reference: "refs/heads/c", OldOID: zeroOID, NewOID: oidC}})
	require.NoError(t, err)
	require.Equal(t, []string{"a@1", "c@2"}, sequencesAndReferences(t, repoPath))
}

// TestAppend_checksTheLastEntryOfACacheOfTheSameSize: a cache without a tail and a file of the same
// size are not enough. The line before the cache's end must still hold the cache's last entry,
// otherwise the state is loaded again.
func TestAppend_checksTheLastEntryOfACacheOfTheSameSize(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)
	path := Path(repoPath)
	_, err := Append(repoPath, Meta{Operation: "test"}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)

	// The file now ends with another entry of the same length: sequence 7 instead of 1.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 1, bytes.Count(data, []byte(`"sequence":1,`)))
	require.NoError(t, os.WriteFile(path, bytes.Replace(data, []byte(`"sequence":1,`), []byte(`"sequence":7,`), 1), 0o644))

	_, err = Append(repoPath, Meta{Operation: "test"}, []Update{{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: oidB}})
	require.NoError(t, err)
	require.Equal(t, []string{"a@7", "b@8"}, sequencesAndReferences(t, repoPath), "the append follows the entry in the file")
}

// TestAppend_doesNotTrustACacheWithATail is the overlap of two processes during a graceful
// restart. Process A's cache knows of bytes after its last valid entry, which it would cut off on
// its next append. Process B cut them off itself and appended a durable entry, which ends at the
// same size. A must not take the size for an unchanged file: it would cut off B's entry.
func TestAppend_doesNotTrustACacheWithATail(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)
	path := Path(repoPath)
	_, err := Append(repoPath, Meta{Operation: "test"}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)

	statesMu.Lock()
	processA := states[path]
	delete(states, path)
	statesMu.Unlock()
	afterA := processA.validEnd

	// Process B appends with its own state.
	_, err = Append(repoPath, Meta{Operation: "test"}, []Update{{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: oidB}})
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)

	// Process A is back, and its cache takes everything after its entry for a tail of the size the
	// file has now.
	statesMu.Lock()
	states[path] = processA
	statesMu.Unlock()
	processA.mu.Lock()
	processA.fileSize = info.Size()
	processA.validEnd = afterA
	processA.mu.Unlock()

	require.Equal(t, []string{"a@1", "b@2"}, sequencesAndReferences(t, repoPath), "a reader of A sees B's entry")

	processA.mu.Lock()
	processA.fileSize = info.Size()
	processA.validEnd = afterA
	processA.mu.Unlock()
	_, err = Append(repoPath, Meta{Operation: "test"}, []Update{{Reference: "refs/heads/c", OldOID: zeroOID, NewOID: oidC}})
	require.NoError(t, err)
	require.Equal(t, []string{"a@1", "b@2", "c@3"}, sequencesAndReferences(t, repoPath), "A's append keeps B's entry")
}
