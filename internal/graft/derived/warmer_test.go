package derived

import (
	"context"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/stretchr/testify/require"
)

// registerToyWarmer registers fn under name for the duration of the test. Warmers
// are process-wide, so every test removes its own. The tests that use warmers are
// not parallel, so they never see each other's registrations.
func registerToyWarmer(t *testing.T, name string, fn WarmFunc) {
	t.Helper()

	RegisterWarmer(name, fn)
	t.Cleanup(func() {
		warmers.Lock()
		defer warmers.Unlock()
		delete(warmers.byName, name)
	})
}

// toyWarmer stores a value for every commit under the kind "toy" version 1. The value
// is the commit ID, so a test can check what was computed.
func toyWarmer(_ context.Context, env WarmEnv, commits []git.ObjectID) error {
	for _, commit := range commits {
		if err := env.Store.Kind("toy", 1).Put(commit, []byte("value-"+commit.String())); err != nil {
			return err
		}
	}
	return nil
}

func TestRegisterWarmer_RegistersAndListsSorted(t *testing.T) {
	registerToyWarmer(t, "toy-zeta", toyWarmer)
	registerToyWarmer(t, "toy-alpha", toyWarmer)

	names := Warmers()
	require.Contains(t, names, "toy-alpha")
	require.Contains(t, names, "toy-zeta")
	require.IsIncreasing(t, names, "warmer names are sorted")

	_, ok := lookupWarmer("toy-alpha")
	require.True(t, ok)
}

func TestRegisterWarmer_PanicsOnDuplicateInvalidAndNil(t *testing.T) {
	registerToyWarmer(t, "toy-dup", toyWarmer)

	require.Panics(t, func() { RegisterWarmer("toy-dup", toyWarmer) }, "duplicate registration")
	require.Panics(t, func() { RegisterWarmer("Bad_Name", toyWarmer) }, "invalid name")
	require.Panics(t, func() { RegisterWarmer("toy-nil", nil) }, "nil function")
}

func TestKindStore_RoundTripAndVersionIsolation(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	commit := fakeOID(1)

	v1 := store.Kind("toy", 1)
	_, found, err := v1.Get(commit)
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, v1.Put(commit, []byte("one")))

	got, found, err := v1.Get(commit)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []byte("one"), got)

	_, found, err = store.Kind("toy", 2).Get(commit)
	require.NoError(t, err)
	require.False(t, found, "a bumped version never reads the old values")

	_, found, err = store.Kind("other", 1).Get(commit)
	require.NoError(t, err)
	require.False(t, found, "kinds never share values")
}

func TestKindStore_InvalidNameIsAnError(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	require.Error(t, store.Kind("Bad_Name", 1).Put(fakeOID(1), []byte("x")))
	_, _, err := store.Kind("Bad_Name", 1).Get(fakeOID(1))
	require.Error(t, err)
}

func TestKindStore_NilStoreDiscardsWrites(t *testing.T) {
	t.Parallel()

	var store *Store
	kind := store.Kind("toy", 1)

	require.NoError(t, kind.Put(fakeOID(1), []byte("x")))
	_, found, err := kind.Get(fakeOID(1))
	require.NoError(t, err)
	require.False(t, found)
}
