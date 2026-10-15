package changelog

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLastSequence(t *testing.T) {
	t.Parallel()

	t.Run("missing log", func(t *testing.T) {
		t.Parallel()

		last, err := LastSequence(newRepoPath(t))
		require.NoError(t, err)
		require.Zero(t, last)
	})

	t.Run("after appends, from the cache", func(t *testing.T) {
		t.Parallel()

		repoPath := newRepoPath(t)
		_, err := Append(repoPath, Meta{}, []Update{
			{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA},
			{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: oidB},
		})
		require.NoError(t, err)

		state := cachedState(t, Path(repoPath))
		state.mu.Lock()
		rescans := state.rescans
		state.mu.Unlock()

		last, err := LastSequence(repoPath)
		require.NoError(t, err)
		require.Equal(t, uint64(2), last)

		state.mu.Lock()
		defer state.mu.Unlock()
		require.Equal(t, rescans, state.rescans, "a cached log is not read again")
	})

	t.Run("entries of another process and a torn tail", func(t *testing.T) {
		t.Parallel()

		repoPath := newRepoPath(t)
		_, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
		require.NoError(t, err)

		// Another process appends entry 2 and crashes while writing entry 3.
		f, err := os.OpenFile(Path(repoPath), os.O_APPEND|os.O_WRONLY, 0o644)
		require.NoError(t, err)
		_, err = f.WriteString(`{"sequence":2,"reference":"refs/heads/x","old_oid":"` + zeroOID + `","new_oid":"` + oidB + `"}` + "\n" + `{"sequence":3,"refer`)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		last, err := LastSequence(repoPath)
		require.NoError(t, err)
		require.Equal(t, uint64(2), last)

		// The cache of the appends is left alone: the next append still finds the
		// torn tail, truncates it and continues the sequences.
		entries, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: oidC}})
		require.NoError(t, err)
		require.Equal(t, uint64(3), entries[0].Sequence)
		require.Len(t, readLines(t, repoPath), 3)

		last, err = LastSequence(repoPath)
		require.NoError(t, err)
		require.Equal(t, uint64(3), last)
	})
}
