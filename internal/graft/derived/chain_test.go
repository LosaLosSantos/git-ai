package derived

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// fakeOID returns a well-formed object ID for the fake commit with the given number.
func fakeOID(i int) git.ObjectID {
	return git.ObjectID(fmt.Sprintf("%040x", i+1))
}

// fakeChain is a history of fake commits. Commit i links to commit i+1 unless its
// result is set, in which case the walk ends there with that result. Steps are
// counted, so tests can check how far a walk went.
type fakeChain struct {
	length int
	// results maps a commit number to the result that ends the walk at it.
	results map[int]git.ObjectID
	// steps is the number of links computed.
	steps int
	// fail, when set, is returned by the step of that commit.
	fail map[int]error
}

func (c *fakeChain) step(_ context.Context, commit git.ObjectID) (git.ObjectID, git.ObjectID, error) {
	c.steps++

	n, err := strconv.ParseInt(commit.String(), 16, 64)
	if err != nil {
		return "", "", err
	}
	i := int(n) - 1

	if err := c.fail[i]; err != nil {
		return "", "", err
	}
	if result, ok := c.results[i]; ok {
		return "", result, nil
	}
	if i+1 >= c.length {
		return "", "", fmt.Errorf("commit %d has no link", i)
	}
	return fakeOID(i + 1), "", nil
}

func newChainWalk(t *testing.T, store *Store, c *fakeChain) chainWalk {
	t.Helper()

	return chainWalk{
		kind:   "fake-chain",
		key:    func(commit git.ObjectID) string { return Key("fake-chain", commit, "path") },
		step:   c.step,
		store:  store,
		logger: testhelper.NewLogger(t),
	}
}

func TestChainWalk_ComputesResultAndStoresCheckpoints(t *testing.T) {
	t.Parallel()

	const length = 200
	chain := &fakeChain{length: length, results: map[int]git.ObjectID{length - 1: fakeOID(length - 1)}}
	store := NewStore(t.TempDir())

	got, err := newChainWalk(t, store, chain).run(testhelper.Context(t), fakeOID(0))
	require.NoError(t, err)
	require.Equal(t, fakeOID(length-1), got)
	require.Equal(t, length, chain.steps)

	// Members 0, 64 and 128 are checkpoints, 192 is one too, and the result commit
	// is stored as well. Other members are not stored.
	for _, i := range []int{0, 64, 128, 192, length - 1} {
		value, found, err := store.Get("fake-chain", Key("fake-chain", fakeOID(i), "path"))
		require.NoError(t, err)
		require.True(t, found, "member %d must be stored", i)

		decoded, valid := decodeValue(value)
		require.True(t, valid)
		require.Equal(t, fakeOID(length-1), decoded)
	}
	for _, i := range []int{1, 63, 65, 100} {
		_, found, err := store.Get("fake-chain", Key("fake-chain", fakeOID(i), "path"))
		require.NoError(t, err)
		require.False(t, found, "member %d must not be stored", i)
	}
}

func TestChainWalk_StoredValueEndsWalk(t *testing.T) {
	t.Parallel()

	const length = 200
	store := NewStore(t.TempDir())
	walk := newChainWalk(t, store, &fakeChain{length: length, results: map[int]git.ObjectID{length - 1: fakeOID(length - 1)}})
	require.NoError(t, store.Put("fake-chain", Key("fake-chain", fakeOID(100), "path"), encodeValue(fakeOID(42))))

	chain := &fakeChain{length: length, results: map[int]git.ObjectID{length - 1: fakeOID(length - 1)}}
	walk.step = chain.step

	got, err := walk.run(testhelper.Context(t), fakeOID(0))
	require.NoError(t, err)
	require.Equal(t, fakeOID(42), got, "the stored value of commit 100 answers the walk")
	require.Equal(t, 100, chain.steps, "only the commits before the stored one are computed")
}

func TestChainWalk_AbsentResultIsNotStoredAsNewEntry(t *testing.T) {
	t.Parallel()

	const length = 10
	// The root of this chain has no link and its result is absent.
	chain := &fakeChain{length: length, results: map[int]git.ObjectID{length - 1: ""}}
	store := NewStore(t.TempDir())

	for i := 0; i < 3; i++ {
		got, err := newChainWalk(t, store, chain).run(testhelper.Context(t), fakeOID(0))
		require.NoError(t, err)
		require.Empty(t, got)
	}

	_, found, err := store.Get("fake-chain", Key("fake-chain", fakeOID(0), "path"))
	require.NoError(t, err)
	require.False(t, found, "an absent result must not create entries")
}

func TestChainWalk_CorruptEntryIsRecomputedAndReplaced(t *testing.T) {
	t.Parallel()

	const length = 10
	store := NewStore(t.TempDir())
	key := Key("fake-chain", fakeOID(3), "path")
	require.NoError(t, store.Put("fake-chain", key, []byte("not an object id")))

	chain := &fakeChain{length: length, results: map[int]git.ObjectID{length - 1: fakeOID(length - 1)}}
	got, err := newChainWalk(t, store, chain).run(testhelper.Context(t), fakeOID(0))
	require.NoError(t, err)
	require.Equal(t, fakeOID(length-1), got)

	value, found, err := store.Get("fake-chain", key)
	require.NoError(t, err)
	require.True(t, found)
	decoded, valid := decodeValue(value)
	require.True(t, valid, "the corrupt entry is replaced by a valid one")
	require.Equal(t, fakeOID(length-1), decoded)
}

func TestChainWalk_StepErrorFailsWalk(t *testing.T) {
	t.Parallel()

	const length = 10
	stepErr := errors.New("boom")
	chain := &fakeChain{length: length, fail: map[int]error{5: stepErr}}

	_, err := newChainWalk(t, NewStore(t.TempDir()), chain).run(testhelper.Context(t), fakeOID(0))
	require.ErrorIs(t, err, stepErr)
}

func TestChainWalk_NilStoreComputesWithoutCaching(t *testing.T) {
	t.Parallel()

	const length = 5
	chain := &fakeChain{length: length, results: map[int]git.ObjectID{length - 1: fakeOID(length - 1)}}

	got, err := newChainWalk(t, nil, chain).run(testhelper.Context(t), fakeOID(0))
	require.NoError(t, err)
	require.Equal(t, fakeOID(length-1), got)
}
