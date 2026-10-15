package changedpaths

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// unlimited is a limit that no test history reaches.
const unlimited = 1 << 30

// collect returns the IDs of the commits PathHistory reports.
func collect(t *testing.T, index *Index, start, path string, limit int) []string {
	t.Helper()

	var ids []string
	require.NoError(t, index.PathHistory(context.Background(), start, path, limit, func(c Commit) error {
		ids = append(ids, c.ID)
		return nil
	}))
	return ids
}

// chain builds a history with a merge:
//
//	r: a/x=1            (root)
//	c2: a/x=2
//	c3: a/x=2, b=1      (b added)
//	c4: a/x=2, b=1      (empty commit)
//	c5: merge of c4 and c2, b=2 (second parent differs in a/x only)
func chain(f *fakeObjects) (r, c2, c3, c4, c5 string) {
	t1 := f.tree(subtree("a", f.tree(blob("x", "1"))))
	t2 := f.tree(subtree("a", f.tree(blob("x", "2"))))
	t3 := f.tree(subtree("a", f.tree(blob("x", "2"))), blob("b", "1"))
	t5 := f.tree(subtree("a", f.tree(blob("x", "2"))), blob("b", "2"))

	r = f.commit(t1)
	c2 = f.commit(t2, r)
	c3 = f.commit(t3, c2)
	c4 = f.commit(t3, c3)
	c5 = f.commit(t5, c4, c2)
	return r, c2, c3, c4, c5
}

func TestIndex_PathHistoryFollowsFirstParents(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	r, c2, c3, _, c5 := chain(f)
	index := NewIndex(f, newMemStore())

	require.Equal(t, []string{c2, r}, collect(t, index, c5, "a/x", unlimited))
	require.Equal(t, []string{c5, c3}, collect(t, index, c5, "b", unlimited))
	require.Equal(t, []string{c2, r}, collect(t, index, c2, "a", unlimited))
	require.Equal(t, []string{r}, collect(t, index, r, "a/x", unlimited))
	require.Empty(t, collect(t, index, c5, "a/y", unlimited))
	require.Equal(t, []string{c5}, collect(t, index, c5, "b", 1))
}

func TestIndex_SummariesAreStored(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	_, _, _, _, c5 := chain(f)
	store := newMemStore()

	// The walk visits c5, c4, c3, c2 and the root, so five summaries are computed.
	index := NewIndex(f, store)
	collect(t, index, c5, "b", unlimited)
	require.Equal(t, int64(5), index.Computed())

	// A walk over the same history reads the summaries back and diffs nothing.
	collect(t, index, c5, "a/x", unlimited)
	require.Equal(t, int64(5), index.Computed())

	// A new index over the same store also reads the summaries back.
	other := NewIndex(f, store)
	collect(t, other, c5, "a/x", unlimited)
	require.Zero(t, other.Computed())
}

func TestIndex_CorruptEntriesAreRecomputed(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	_, _, c3, _, _ := chain(f)
	store := newMemStore()
	store.data[c3] = []byte{9, 9, 9}

	index := NewIndex(f, store)
	s, err := index.CommitSummary(context.Background(), c3)
	require.NoError(t, err)
	require.True(t, s.Contains("b"))
	require.Equal(t, int64(1), index.Computed())

	decoded, err := decodeSummary(store.data[c3])
	require.NoError(t, err)
	require.True(t, decoded.Contains("b"))
}

func TestIndex_OverflowIsAnsweredByPathDiff(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	r := f.commit(f.tree(subtree("a", f.tree(blob("x", "1"), blob("y", "1")))))
	c2 := f.commit(f.tree(subtree("a", f.tree(blob("x", "2"), blob("y", "2")))), r)

	index := NewIndex(f, newMemStore())
	index.maxPaths = 1 // both commits change two files, so both overflow

	s, err := index.CommitSummary(context.Background(), c2)
	require.NoError(t, err)
	require.True(t, s.Overflow)

	require.Equal(t, []string{c2, r}, collect(t, index, c2, "a/y", unlimited))
	require.Equal(t, []string{c2}, collect(t, index, c2, "a/x", 1))
	require.Empty(t, collect(t, index, c2, "a/z", unlimited))
}

func TestIndex_InvalidPaths(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	_, _, _, _, c5 := chain(f)
	index := NewIndex(f, nil)

	for _, path := range []string{"..", "../a", "a/../../b", "/a", "//a"} {
		require.False(t, Supports(path), path)
		err := index.PathHistory(context.Background(), c5, path, unlimited, func(Commit) error { return nil })
		require.ErrorIs(t, err, ErrInvalidPath, path)
	}
}

func TestIndex_PathNormalization(t *testing.T) {
	t.Parallel()

	f := newFakeObjects()
	r, c2, c3, _, c5 := chain(f)
	index := NewIndex(f, nil)

	// The Deriver cleans paths and maps the empty path, "/" and "." to the root,
	// and so must the index.
	require.Equal(t, []string{c5, c3, c2, r}, collect(t, index, c5, "", unlimited))
	for path, want := range map[string]string{"/": "", ".": "", "a/": "a", "./a": "a", "a//x": "a/x", "a/../b": "b"} {
		require.True(t, Supports(path), path)
		require.Equal(t, collect(t, index, c5, want, unlimited), collect(t, index, c5, path, unlimited), path)
	}
}
