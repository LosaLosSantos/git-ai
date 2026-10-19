package changelog

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The tests of this file replace syncFile and truncateFile, so they are not parallel.

func durableUpdate(reference string) []Update {
	return []Update{{
		Reference: reference,
		OldOID:    "0000000000000000000000000000000000000000",
		NewOID:    "1111111111111111111111111111111111111111",
	}}
}

// replaceSyncFile replaces syncFile for the test.
func replaceSyncFile(t *testing.T, fn func(f *os.File) error) {
	t.Helper()
	original := syncFile
	syncFile = fn
	t.Cleanup(func() { syncFile = original })
}

func references(t *testing.T, repoPath string, after uint64) []string {
	t.Helper()

	var refs []string
	require.NoError(t, Scan(repoPath, after, func(e Entry) (bool, error) {
		refs = append(refs, e.Reference)
		return true, nil
	}))
	return refs
}

// TestAppendGroupWritesOnceInOrder records three transactions with one fsync: their entries
// follow each other in the order of the records, with consecutive sequences, and equal what Append
// records for the same updates.
func TestAppendGroupWritesOnceInOrder(t *testing.T) {
	repoPath := t.TempDir()
	_, err := Append(repoPath, Meta{Operation: "first"}, durableUpdate("refs/heads/a"))
	require.NoError(t, err)

	var syncs atomic.Int32
	replaceSyncFile(t, func(f *os.File) error {
		syncs.Add(1)
		return f.Sync()
	})

	recordedAt := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	records := []Record{
		{Meta: Meta{Operation: "one", Actor: "x", RecordedAt: recordedAt}, Updates: append(durableUpdate("refs/heads/b"), durableUpdate("refs/heads/c")...)},
		{Meta: Meta{Operation: "empty"}},
		{Meta: Meta{Operation: "two", Actor: "y", RecordedAt: recordedAt, Rewritten: true}, Updates: durableUpdate("refs/heads/d")},
	}
	entries, err := AppendGroup(repoPath, records)
	require.NoError(t, err)
	require.EqualValues(t, 1, syncs.Load(), "one fsync for the group")

	require.Len(t, entries, 3)
	require.Len(t, entries[0], 2)
	require.Empty(t, entries[1])
	require.Len(t, entries[2], 1)
	require.Equal(t, []uint64{2, 3}, []uint64{entries[0][0].Sequence, entries[0][1].Sequence})
	require.EqualValues(t, 4, entries[2][0].Sequence)
	require.Equal(t, "two", entries[2][0].Operation)
	require.True(t, entries[2][0].Rewritten)

	listed, err := List(repoPath, 1, 10)
	require.NoError(t, err)
	require.Equal(t, append(entries[0], entries[2]...), listed)

	// Append records the same entries as a group of one record.
	other := t.TempDir()
	_, err = Append(other, Meta{Operation: "first"}, durableUpdate("refs/heads/a"))
	require.NoError(t, err)
	single, err := Append(other, records[0].Meta, records[0].Updates)
	require.NoError(t, err)
	require.Equal(t, entries[0], single)
}

// TestReadersSeeEntriesOnlyOnceDurable holds an append between its write and its fsync: the
// readers of the log, in this process and from a state loaded anew, do not return the new entry
// before the fsync returns.
func TestReadersSeeEntriesOnlyOnceDurable(t *testing.T) {
	repoPath := t.TempDir()
	_, err := Append(repoPath, Meta{Operation: "test"}, durableUpdate("refs/heads/a"))
	require.NoError(t, err)

	inSync := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	replaceSyncFile(t, func(f *os.File) error {
		once.Do(func() {
			close(inSync)
			<-release
		})
		return f.Sync()
	})

	appended := make(chan error, 1)
	go func() {
		_, err := Append(repoPath, Meta{Operation: "test"}, durableUpdate("refs/heads/b"))
		appended <- err
	}()
	<-inSync

	// The bytes of the entry are in the file now.
	data, err := os.ReadFile(Path(repoPath))
	require.NoError(t, err)
	require.Contains(t, string(data), "refs/heads/b")

	// The reader comes without this process's state, as a reader of another process does: it
	// does not wait for the state's mutex but for the log's lock file, which the append holds.
	statesMu.Lock()
	delete(states, Path(repoPath))
	statesMu.Unlock()

	type result struct {
		refs []string
		last uint64
		err  error
	}
	read := make(chan result, 1)
	go func() {
		var r result
		var entries []Entry
		if entries, r.err = List(repoPath, 0, 10); r.err == nil {
			for _, entry := range entries {
				r.refs = append(r.refs, entry.Reference)
			}
			r.last, r.err = LastSequence(repoPath)
		}
		read <- r
	}()

	readEarly := false
	select {
	case r := <-read:
		readEarly = true
		require.NoError(t, r.err)
		require.Equal(t, []string{"refs/heads/a"}, r.refs, "a reader returned an entry before its fsync")
		require.EqualValues(t, 1, r.last)
	case <-time.After(200 * time.Millisecond):
		// The reader waits for the append, which is what the test expects.
	}

	close(release)
	require.NoError(t, <-appended)

	if !readEarly {
		// The reader waited for the lock file: it read once the entry was durable.
		r := <-read
		require.NoError(t, r.err)
		require.Equal(t, []string{"refs/heads/a", "refs/heads/b"}, r.refs)
		require.EqualValues(t, 2, r.last)
	}
	require.Equal(t, []string{"refs/heads/a", "refs/heads/b"}, references(t, repoPath, 0))
}

// TestFailedAppendIsNeverVisible is the case of a crash before the fsync, as a failed fsync shows
// it: the entry of the failed append is cut off and never visible, the next append takes its
// sequence for another update, and a reader that follows the log with a cursor misses nothing.
// Before the durable end, a reader could return the entry before its fsync and then skip the one
// that took its sequence.
func TestFailedAppendIsNeverVisible(t *testing.T) {
	repoPath := t.TempDir()
	_, err := Append(repoPath, Meta{Operation: "test"}, durableUpdate("refs/heads/a"))
	require.NoError(t, err)

	var cursor uint64
	require.NoError(t, Scan(repoPath, 0, func(e Entry) (bool, error) { cursor = e.Sequence; return true, nil }))
	require.EqualValues(t, 1, cursor)

	failed := true
	replaceSyncFile(t, func(f *os.File) error {
		if failed {
			failed = false
			return errors.New("injected fsync failure")
		}
		return f.Sync()
	})

	_, err = Append(repoPath, Meta{Operation: "test"}, durableUpdate("refs/heads/lost"))
	require.ErrorContains(t, err, "injected fsync failure")

	data, err := os.ReadFile(Path(repoPath))
	require.NoError(t, err)
	require.NotContains(t, string(data), "refs/heads/lost", "the bytes of the failed append are cut off")
	require.Empty(t, references(t, repoPath, cursor))
	last, err := LastSequence(repoPath)
	require.NoError(t, err)
	require.EqualValues(t, 1, last)

	// A restart, then the next append takes sequence 2.
	dropState(repoPath)
	entries, err := Append(repoPath, Meta{Operation: "test"}, durableUpdate("refs/heads/c"))
	require.NoError(t, err)
	require.EqualValues(t, 2, entries[0].Sequence)

	require.Equal(t, []string{"refs/heads/c"}, references(t, repoPath, cursor))
}

// TestFailedAppendThatCannotBeCutOffIsIgnored fails the fsync of an append and the truncation
// that would cut its bytes off: they stay in the file, but no reader returns them, and the next
// append truncates them and takes their sequence.
func TestFailedAppendThatCannotBeCutOffIsIgnored(t *testing.T) {
	repoPath := t.TempDir()
	_, err := Append(repoPath, Meta{Operation: "test"}, durableUpdate("refs/heads/a"))
	require.NoError(t, err)

	failSync := true
	replaceSyncFile(t, func(f *os.File) error {
		if failSync {
			failSync = false
			return errors.New("injected fsync failure")
		}
		return f.Sync()
	})
	originalTruncate := truncateFile
	failTruncate := true
	truncateFile = func(f *os.File, size int64) error {
		if failTruncate {
			failTruncate = false
			return errors.New("injected truncate failure")
		}
		return originalTruncate(f, size)
	}
	t.Cleanup(func() { truncateFile = originalTruncate })

	_, err = Append(repoPath, Meta{Operation: "test"}, durableUpdate("refs/heads/lost"))
	require.ErrorContains(t, err, "injected fsync failure")

	data, err := os.ReadFile(Path(repoPath))
	require.NoError(t, err)
	require.Contains(t, string(data), "refs/heads/lost", "the truncation failed, so the bytes stay")

	require.Equal(t, []string{"refs/heads/a"}, references(t, repoPath, 0))
	last, err := LastSequence(repoPath)
	require.NoError(t, err)
	require.EqualValues(t, 1, last)

	entries, err := Append(repoPath, Meta{Operation: "test"}, durableUpdate("refs/heads/c"))
	require.NoError(t, err)
	require.EqualValues(t, 2, entries[0].Sequence)

	data, err = os.ReadFile(Path(repoPath))
	require.NoError(t, err)
	require.NotContains(t, string(data), "refs/heads/lost")
	require.Equal(t, []string{"refs/heads/a", "refs/heads/c"}, references(t, repoPath, 0))
}

// TestLoadingTheLogFsyncsItFirst reads a log whose last entry a crashed process wrote without
// fsyncing it: the reader fsyncs the file before it returns the entry, so the entry is durable
// once it is visible.
func TestLoadingTheLogFsyncsItFirst(t *testing.T) {
	repoPath := t.TempDir()
	_, err := Append(repoPath, Meta{Operation: "test"}, durableUpdate("refs/heads/a"))
	require.NoError(t, err)

	// What a crashed process left: a complete line after the last fsync.
	entries, err := Append(t.TempDir(), Meta{Operation: "test"}, durableUpdate("refs/heads/b"))
	require.NoError(t, err)
	entries[0].Sequence = 2
	line, err := entries[0].MarshalJSON()
	require.NoError(t, err)
	f, err := os.OpenFile(Path(repoPath), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.Write(append(line, '\n'))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	dropState(repoPath)

	var syncs atomic.Int32
	replaceSyncFile(t, func(f *os.File) error {
		syncs.Add(1)
		return f.Sync()
	})

	require.Equal(t, []string{"refs/heads/a", "refs/heads/b"}, references(t, repoPath, 0))
	require.EqualValues(t, 1, syncs.Load(), "the reader fsyncs the log when it loads it")

	// The state is loaded now: further reads do not fsync.
	last, err := LastSequence(repoPath)
	require.NoError(t, err)
	require.EqualValues(t, 2, last)
	require.EqualValues(t, 1, syncs.Load())
}
