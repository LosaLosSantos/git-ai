package graph

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"math/rand"
	"sort"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/stretchr/testify/require"
)

// fakeSource is an in-memory history. It counts the reads of parents, and it can fail
// a number of reads before it answers, to test that a failed walk does not poison the
// index.
type fakeSource struct {
	parents  map[git.ObjectID][]git.ObjectID
	failures int
	calls    int
}

func (s *fakeSource) Parents(ctx context.Context, id git.ObjectID) ([]git.ObjectID, error) {
	s.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.failures > 0 {
		s.failures--
		return nil, errors.New("transient read failure")
	}

	parents, ok := s.parents[id]
	if !ok {
		return nil, fmt.Errorf("unknown commit %s", id)
	}
	return parents, nil
}

func commitID(i int) git.ObjectID { return git.ObjectID(fmt.Sprintf("c%d", i)) }

// hexID returns a valid object ID of the SHA-1 size for commit i. Stored entries
// encode their skip pointers as raw object IDs, so the stored tests use these.
func hexID(i int) git.ObjectID { return git.ObjectID(fmt.Sprintf("%040x", i+1)) }

// noStore returns the store of an Index that persists nothing.
func noStore() derived.KindStore { return (*derived.Store)(nil).Kind(kindName, kindVersion) }

// randomDAG returns n commits c0..c(n-1), see randomDAGWith.
func randomDAG(rnd *rand.Rand, n int) map[git.ObjectID][]git.ObjectID {
	return randomDAGWith(rnd, n, commitID)
}

// randomDAGWith returns the parents of n commits named by id. Every commit has a
// first parent that is often the previous commit, and some have a second parent. A
// few commits start a new root, so that the history can be disconnected.
func randomDAGWith(rnd *rand.Rand, n int, id func(int) git.ObjectID) map[git.ObjectID][]git.ObjectID {
	parents := make(map[git.ObjectID][]git.ObjectID, n)
	for i := 0; i < n; i++ {
		if i == 0 || rnd.Float64() < 0.05 {
			parents[id(i)] = nil
			continue
		}

		first := i - 1
		if rnd.Intn(2) == 0 {
			first = rnd.Intn(i)
		}
		list := []git.ObjectID{id(first)}
		if rnd.Intn(3) == 0 {
			if second := rnd.Intn(i); second != first {
				list = append(list, id(second))
			}
		}
		parents[id(i)] = list
	}
	return parents
}

// bruteAncestors returns every commit reachable from id, including id itself.
func bruteAncestors(parents map[git.ObjectID][]git.ObjectID, id git.ObjectID) map[git.ObjectID]bool {
	seen := map[git.ObjectID]bool{id: true}
	stack := []git.ObjectID{id}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, parent := range parents[current] {
			if !seen[parent] {
				seen[parent] = true
				stack = append(stack, parent)
			}
		}
	}
	return seen
}

// ancestorSets returns the ancestors of every commit of parents.
func ancestorSets(parents map[git.ObjectID][]git.ObjectID) map[git.ObjectID]map[git.ObjectID]bool {
	sets := make(map[git.ObjectID]map[git.ObjectID]bool, len(parents))
	for id := range parents {
		sets[id] = bruteAncestors(parents, id)
	}
	return sets
}

// bruteMergeBases returns the common ancestors of a and b that no other common
// ancestor reaches, sorted by ID.
func bruteMergeBases(parents map[git.ObjectID][]git.ObjectID, a, b git.ObjectID) []git.ObjectID {
	return bruteMergeBasesWith(ancestorSets(parents), a, b)
}

// bruteMergeBasesWith is bruteMergeBases with the ancestor sets computed by the caller.
func bruteMergeBasesWith(sets map[git.ObjectID]map[git.ObjectID]bool, a, b git.ObjectID) []git.ObjectID {
	var common []git.ObjectID
	for id := range sets[a] {
		if sets[b][id] {
			common = append(common, id)
		}
	}

	var best []git.ObjectID
	for _, c := range common {
		redundant := false
		for _, d := range common {
			if d != c && sets[d][c] {
				redundant = true
				break
			}
		}
		if !redundant {
			best = append(best, c)
		}
	}

	sort.Slice(best, func(i, j int) bool { return best[i] < best[j] })
	return best
}

func TestIndex_Generation(t *testing.T) {
	t.Parallel()

	src := &fakeSource{parents: map[git.ObjectID][]git.ObjectID{
		"root": nil,
		"a":    {"root"},
		"b":    {"root"},
		"m":    {"a", "b"},
		"n":    {"m"},
	}}
	ix := NewIndex(src, noStore())

	for id, expected := range map[git.ObjectID]uint32{"root": 1, "a": 2, "b": 2, "m": 3, "n": 4} {
		generation, err := ix.Generation(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, expected, generation, "generation of %s", id)
	}
}

func TestIndex_SkipPointers(t *testing.T) {
	t.Parallel()

	const length = 300
	src := &fakeSource{parents: make(map[git.ObjectID][]git.ObjectID, length)}
	for i := 0; i < length; i++ {
		if i == 0 {
			src.parents[commitID(i)] = nil
		} else {
			src.parents[commitID(i)] = []git.ObjectID{commitID(i - 1)}
		}
	}

	ix := NewIndex(src, noStore())
	_, err := ix.Generation(context.Background(), commitID(length-1))
	require.NoError(t, err)

	// The first-parent depth of commit i is i, so it has bits.Len(i) pointers, the
	// pointer k of which is the commit 2^k steps back.
	for i := 0; i < length; i++ {
		e, ok := ix.cache.Get(commitID(i))
		require.True(t, ok, "entry of c%d is cached", i)
		require.Len(t, e.skips, bits.Len(uint(i)), "pointers of c%d", i)
		for k, skip := range e.skips {
			require.Equal(t, commitID(i-(1<<k)), skip, "pointer %d of c%d", k, i)
		}
	}
}

func TestIndex_IsAncestorMatchesBruteForce(t *testing.T) {
	t.Parallel()

	for seed := int64(1); seed <= 5; seed++ {
		parents := randomDAG(rand.New(rand.NewSource(seed)), 60)
		ix := NewIndex(&fakeSource{parents: parents}, noStore())
		ctx := context.Background()

		for i := 0; i < 60; i++ {
			reachable := bruteAncestors(parents, commitID(i))
			for j := 0; j < 60; j++ {
				got, err := ix.IsAncestor(ctx, commitID(j), commitID(i))
				require.NoError(t, err)
				require.Equal(t, reachable[commitID(j)], got, "seed %d: IsAncestor(c%d, c%d)", seed, j, i)
			}
		}
	}
}

func TestIndex_MergeBasesMatchBruteForce(t *testing.T) {
	t.Parallel()

	for seed := int64(1); seed <= 5; seed++ {
		parents := randomDAG(rand.New(rand.NewSource(seed)), 60)
		ix := NewIndex(&fakeSource{parents: parents}, noStore())
		ctx := context.Background()

		for i := 0; i < 60; i++ {
			for j := i; j < 60; j++ {
				expected := bruteMergeBases(parents, commitID(i), commitID(j))

				got, err := ix.MergeBases(ctx, commitID(i), commitID(j))
				require.NoError(t, err)
				require.Equal(t, expected, got, "seed %d: MergeBases(c%d, c%d)", seed, i, j)

				one, err := ix.MergeBase(ctx, commitID(i), commitID(j))
				require.NoError(t, err)
				if len(expected) == 0 {
					require.Empty(t, one)
				} else {
					require.Equal(t, expected[0], one)
				}
			}
		}
	}
}

// TestIndex_RandomDAGWithMergesMatchesBruteForce compares the answers on histories of
// more than 200 commits with many merges, for sampled pairs of commits.
func TestIndex_RandomDAGWithMergesMatchesBruteForce(t *testing.T) {
	t.Parallel()

	const n = 220
	ctx := context.Background()

	for seed := int64(1); seed <= 2; seed++ {
		rnd := rand.New(rand.NewSource(seed))
		parents := randomDAGWith(rnd, n, hexID)

		merges := 0
		for _, list := range parents {
			if len(list) > 1 {
				merges++
			}
		}
		require.GreaterOrEqual(t, merges, 20, "seed %d has merges", seed)

		sets := ancestorSets(parents)
		ix := NewIndex(&fakeSource{parents: parents}, noStore())

		for k := 0; k < 500; k++ {
			i, j := rnd.Intn(n), rnd.Intn(n)

			isAncestor, err := ix.IsAncestor(ctx, hexID(i), hexID(j))
			require.NoError(t, err)
			require.Equal(t, sets[hexID(j)][hexID(i)], isAncestor, "seed %d: IsAncestor(%d, %d)", seed, i, j)

			bases, err := ix.MergeBases(ctx, hexID(i), hexID(j))
			require.NoError(t, err)
			require.Equal(t, bruteMergeBasesWith(sets, hexID(i), hexID(j)), bases, "seed %d: MergeBases(%d, %d)", seed, i, j)
		}
	}
}

func TestIndex_SourceFailuresAreRetried(t *testing.T) {
	t.Parallel()

	parents := randomDAG(rand.New(rand.NewSource(7)), 60)
	src := &fakeSource{parents: parents}
	ix := NewIndex(src, noStore())
	ctx := context.Background()

	// Each failure aborts a walk. Retries must eventually succeed with the right answer
	// rather than report a cycle from the half-expanded entries of the failed walks.
	src.failures = 25
	var got bool
	var err error
	for attempt := 0; attempt < 100; attempt++ {
		got, err = ix.IsAncestor(ctx, commitID(0), commitID(59))
		if err == nil {
			break
		}
		require.NotContains(t, err.Error(), "cycle")
	}
	require.NoError(t, err)
	require.Equal(t, bruteAncestors(parents, commitID(59))[commitID(0)], got)
}

func TestIndex_CycleIsReported(t *testing.T) {
	t.Parallel()

	src := &fakeSource{parents: map[git.ObjectID][]git.ObjectID{
		"a": {"b"},
		"b": {"a"},
	}}

	_, err := NewIndex(src, noStore()).IsAncestor(context.Background(), "a", "b")
	require.ErrorContains(t, err, "cycle")
}

func TestIndex_CanceledContext(t *testing.T) {
	t.Parallel()

	src := &fakeSource{parents: map[git.ObjectID][]git.ObjectID{"a": nil, "b": {"a"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewIndex(src, noStore()).IsAncestor(ctx, "a", "b")
	require.ErrorIs(t, err, context.Canceled)
}

// TestIndex_DeepHistoryWithoutRecursion uses a history longer than the cache, so that
// evictions happen, and checks that the cache stays bounded.
func TestIndex_DeepHistoryWithoutRecursion(t *testing.T) {
	t.Parallel()

	const length = 100000
	parents := make(map[git.ObjectID][]git.ObjectID, length+1)
	for i := 0; i < length; i++ {
		if i == 0 {
			parents[commitID(i)] = nil
		} else {
			parents[commitID(i)] = []git.ObjectID{commitID(i - 1)}
		}
	}
	// A branch that forks off the middle of the chain.
	parents["branch"] = []git.ObjectID{commitID(length / 2)}

	ix := NewIndex(&fakeSource{parents: parents}, noStore())
	ctx := context.Background()

	ancestor, err := ix.IsAncestor(ctx, commitID(0), commitID(length-1))
	require.NoError(t, err)
	require.True(t, ancestor)

	ancestor, err = ix.IsAncestor(ctx, commitID(length-1), commitID(0))
	require.NoError(t, err)
	require.False(t, ancestor)

	bases, err := ix.MergeBases(ctx, commitID(length-1), "branch")
	require.NoError(t, err)
	require.Equal(t, []git.ObjectID{commitID(length / 2)}, bases)

	require.LessOrEqual(t, ix.cache.Len(), maxCachedEntries)
}

// TestIndex_PersistedEntriesAreReused checks that the entries stored by one Index are
// read back by another one instead of being computed from the parents again.
func TestIndex_PersistedEntriesAreReused(t *testing.T) {
	t.Parallel()

	const n = 220
	ctx := context.Background()
	parents := randomDAGWith(rand.New(rand.NewSource(11)), n, hexID)
	store := derived.NewStore(t.TempDir()).Kind(kindName, kindVersion)
	// The tip is the commit with the longest history of the history, so that the walk
	// it causes is long enough to compare the reads.
	tip := hexID(n - 1)
	for i := 0; i < n; i++ {
		if len(bruteAncestors(parents, hexID(i))) > len(bruteAncestors(parents, tip)) {
			tip = hexID(i)
		}
	}
	require.GreaterOrEqual(t, len(bruteAncestors(parents, tip)), 30)

	first := &fakeSource{parents: parents}
	expected, err := NewIndex(first, store).Generation(ctx, tip)
	require.NoError(t, err)
	require.Greater(t, first.calls, 0)

	history := bruteAncestors(parents, tip)
	for id := range history {
		_, found, err := store.Get(id)
		require.NoError(t, err)
		require.True(t, found, "entry of %s is stored", id)
	}

	second := &fakeSource{parents: parents}
	ix := NewIndex(second, store)
	generation, err := ix.Generation(ctx, tip)
	require.NoError(t, err)
	require.Equal(t, expected, generation)
	require.Zero(t, second.calls, "the stored entries are not computed again")

	sets := ancestorSets(parents)
	rnd := rand.New(rand.NewSource(12))
	for k := 0; k < 200; k++ {
		i, j := rnd.Intn(n), rnd.Intn(n)

		isAncestor, err := ix.IsAncestor(ctx, hexID(i), hexID(j))
		require.NoError(t, err)
		require.Equal(t, sets[hexID(j)][hexID(i)], isAncestor)

		bases, err := ix.MergeBases(ctx, hexID(i), hexID(j))
		require.NoError(t, err)
		require.Equal(t, bruteMergeBasesWith(sets, hexID(i), hexID(j)), bases)
	}
}

// TestIndex_CorruptStoredEntryIsRecomputed checks that an undecodable stored value is
// computed again and replaced.
func TestIndex_CorruptStoredEntryIsRecomputed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	parents := randomDAGWith(rand.New(rand.NewSource(13)), 50, hexID)
	store := derived.NewStore(t.TempDir()).Kind(kindName, kindVersion)
	tip := hexID(49)

	expected, err := NewIndex(&fakeSource{parents: parents}, store).Generation(ctx, tip)
	require.NoError(t, err)

	require.NoError(t, store.Put(tip, []byte("corrupt")))

	ix := NewIndex(&fakeSource{parents: parents}, store)
	generation, err := ix.Generation(ctx, tip)
	require.NoError(t, err)
	require.Equal(t, expected, generation)

	data, found, err := store.Get(tip)
	require.NoError(t, err)
	require.True(t, found)
	_, err = decodeEntry(data)
	require.NoError(t, err, "the corrupt value is replaced")
}

func TestEntryEncoding(t *testing.T) {
	t.Parallel()

	for _, size := range []int{sha1Size, sha256Size} {
		skipID := func(b byte) git.ObjectID {
			return git.ObjectID(hex.EncodeToString(bytes.Repeat([]byte{b}, size)))
		}

		e := &entry{generation: 42, skips: []git.ObjectID{skipID(1), skipID(2), skipID(3)}}
		data, err := encodeEntry(e)
		require.NoError(t, err)
		require.Len(t, data, encodedHeaderSize+3*size)

		decoded, err := decodeEntry(data)
		require.NoError(t, err)
		require.True(t, decoded.done)
		require.Equal(t, e.generation, decoded.generation)
		require.Equal(t, e.skips, decoded.skips)
	}

	root, err := encodeEntry(&entry{generation: 1, done: true})
	require.NoError(t, err)
	require.Len(t, root, encodedHeaderSize)
	decoded, err := decodeEntry(root)
	require.NoError(t, err)
	require.Equal(t, uint32(1), decoded.generation)
	require.Empty(t, decoded.skips)

	_, err = encodeEntry(&entry{generation: 2, skips: []git.ObjectID{"not-hex"}})
	require.Error(t, err)

	for name, data := range map[string][]byte{
		"too short":          {1, 0, 0},
		"generation zero":    {0, 0, 0, 0, 0},
		"root with trailing": {1, 0, 0, 0, 0, 9},
		"uneven pointers":    append([]byte{2, 0, 0, 0, 2}, make([]byte, 21)...),
		"odd pointer size":   append([]byte{2, 0, 0, 0, 1}, make([]byte, 7)...),
	} {
		_, err := decodeEntry(data)
		require.Error(t, err, name)
	}
}
