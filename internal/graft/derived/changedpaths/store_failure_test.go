package changedpaths

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// errDiskFull is the error that failingStore returns for every write.
var errDiskFull = errors.New("disk full")

// failingStore is a Store whose writes fail, as on a full disk or a read-only
// directory. It counts the writes that it was asked for.
type failingStore struct {
	puts int
}

func (s *failingStore) Get(string) ([]byte, bool, error) { return nil, false, nil }

func (s *failingStore) Put(string, []byte) error {
	s.puts++
	return errDiskFull
}

// countingStore is an in-memory Store that counts its writes.
type countingStore struct {
	*memStore
	puts int
}

func (s *countingStore) Put(key string, data []byte) error {
	s.puts++
	return s.memStore.Put(key, data)
}

// TestIndex_FailedWriteIsACacheMiss requires a query to answer correctly when the
// summaries cannot be stored, as the Deriver does. A failed write only costs a
// recomputation later.
func TestIndex_FailedWriteIsACacheMiss(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	r, c2, c3, _, c5 := chain(f)
	store := &failingStore{}
	index := NewIndex(f, store)

	require.Equal(t, []string{c2, r}, collect(t, index, c5, "a/x", unlimited))
	require.Equal(t, []string{c5, c3}, collect(t, index, c5, "b", unlimited))
	require.Equal(t, []string{c5}, collect(t, index, c5, "b", 1))
	// Every write failed, so every query recomputed the summaries it visited: the three
	// queries visit five, five and one commits.
	require.Positive(t, store.puts)
	require.Equal(t, int64(11), index.Computed())
}

// TestIndex_FailedWriteIsReportedByCommitSummary requires warming to report a failed
// write, while the summary itself is still returned.
func TestIndex_FailedWriteIsReportedByCommitSummary(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	_, _, c3, _, _ := chain(f)
	index := NewIndex(f, &failingStore{})

	s, err := index.CommitSummary(context.Background(), c3)
	require.ErrorIs(t, err, errDiskFull)
	require.True(t, s.Contains("b"))
}

// TestIndex_WritesPerQueryAreBounded requires a cold query to store at most
// maxStoredPerQuery summaries, however long the history it walks. The summaries that
// are not stored are computed by the next query, which stores them.
func TestIndex_WritesPerQueryAreBounded(t *testing.T) {
	t.Parallel()

	const length = maxStoredPerQuery + 76

	f := newFakeObjects()
	tip := ""
	for i := 0; i < length; i++ {
		parents := []string{}
		if tip != "" {
			parents = append(parents, tip)
		}
		tip = f.commit(f.tree(subtree("a", f.tree(blob("x", fmt.Sprint(i))))), parents...)
	}

	store := &countingStore{memStore: newMemStore()}
	index := NewIndex(f, store)

	require.Len(t, collect(t, index, tip, "a/x", unlimited), length)
	require.Equal(t, maxStoredPerQuery, store.puts)

	// The second query reads the stored summaries and computes, and stores, the rest.
	require.Len(t, collect(t, index, tip, "a/x", unlimited), length)
	require.Equal(t, length, store.puts)
}

// TestNewIndexFor_NilStorePersistsNothing requires a nil store to disable persistence
// entirely, so that the index diffs the path alone instead of computing summaries
// that are then discarded.
func TestNewIndexFor_NilStorePersistsNothing(t *testing.T) {
	t.Parallel()

	require.Nil(t, NewIndexFor(nil, nil).store)
}
