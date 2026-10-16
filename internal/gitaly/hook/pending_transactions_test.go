package hook

import (
	"fmt"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/stretchr/testify/require"
)

func TestReferencesContained(t *testing.T) {
	t.Parallel()

	zero := gittest.DefaultObjectHash.ZeroOID.String()
	changes := func(refs ...string) []byte {
		var changes string
		for _, ref := range refs {
			changes += fmt.Sprintf("%s %s %s\n", zero, zero, ref)
		}
		return []byte(changes)
	}

	require.True(t, referencesContained(changes("refs/heads/a"), changes("refs/heads/b", "refs/heads/a")))
	require.False(t, referencesContained(changes("refs/heads/a", "refs/heads/c"), changes("refs/heads/a")))

	// Reference names are compared verbatim: Unicode whitespace in a name is not a
	// separator, and trimming it would confuse two different references.
	require.True(t, referencesContained(changes("refs/heads/a\u00a0b"), changes("refs/heads/a\u00a0b")))
	require.False(t, referencesContained(changes("refs/heads/a\u00a0"), changes("refs/heads/a")))
	require.False(t, referencesContained(changes("refs/heads/x\u0085y"), changes("refs/heads/x")))
	require.True(t, referencesContained(changes("refs/heads/caf\xe9"), changes("refs/heads/caf\xe9")))
	require.False(t, referencesContained(changes("refs/heads/caf\xe9"), changes("refs/heads/caf\xe8")))
}

func TestPendingTransactions(t *testing.T) {
	t.Parallel()

	hash := gittest.DefaultObjectHash
	zero := hash.ZeroOID.String()
	old := hash.EmptyTreeOID.String()

	forced := func(refs ...string) []byte {
		var changes string
		for _, ref := range refs {
			changes += fmt.Sprintf("%s %s %s\n", zero, zero, ref)
		}
		return []byte(changes)
	}

	// The packed-refs cleanup and the loose transaction of deleting a packed
	// reference with an expected old value, as git reports them.
	packedPart := forced("refs/heads/packed")
	loosePart := []byte(fmt.Sprintf("%s %s refs/heads/packed\n", old, zero))

	t.Run("packed-refs cleanup is paired with the loose transaction", func(t *testing.T) {
		t.Parallel()

		p := newPendingTransactions()
		p.prepared("git-1", packedPart)
		p.prepared("git-1", loosePart)

		require.True(t, p.committed(hash, "git-1", packedPart), "the packed-refs cleanup is recorded with the loose transaction")
		require.False(t, p.committed(hash, "git-1", loosePart), "the loose transaction is recorded")
		require.Empty(t, p.invocations, "nothing is kept once the transactions are committed")
	})

	t.Run("forced deletions in both backends", func(t *testing.T) {
		t.Parallel()

		p := newPendingTransactions()
		packed := forced("refs/heads/a", "refs/heads/b")
		loose := forced("refs/heads/a", "refs/heads/b", "refs/heads/c")
		p.prepared("git-1", packed)
		p.prepared("git-1", loose)

		require.True(t, p.committed(hash, "git-1", packed))
		require.False(t, p.committed(hash, "git-1", loose))
		require.Empty(t, p.invocations)
	})

	t.Run("deletion of a loose reference", func(t *testing.T) {
		t.Parallel()

		// Git aborts the packed-refs transaction without having prepared it.
		p := newPendingTransactions()
		p.aborted("git-1", forced("refs/heads/loose"))
		p.prepared("git-1", forced("refs/heads/loose"))

		require.False(t, p.committed(hash, "git-1", forced("refs/heads/loose")))
		require.Empty(t, p.invocations)
	})

	t.Run("sequential transactions of one process", func(t *testing.T) {
		t.Parallel()

		p := newPendingTransactions()
		for i := 0; i < 3; i++ {
			p.prepared("git-1", forced("refs/heads/x"))
			require.False(t, p.committed(hash, "git-1", forced("refs/heads/x")), "transaction %d is recorded", i)
		}
		require.Empty(t, p.invocations)
	})

	t.Run("concurrent processes are kept apart", func(t *testing.T) {
		t.Parallel()

		p := newPendingTransactions()
		p.prepared("git-1", forced("refs/heads/x"))
		p.prepared("git-2", forced("refs/heads/x"))

		require.False(t, p.committed(hash, "git-1", forced("refs/heads/x")))
		require.False(t, p.committed(hash, "git-2", forced("refs/heads/x")))
		require.Empty(t, p.invocations)
	})

	t.Run("a transaction that is not a packed-refs cleanup is recorded", func(t *testing.T) {
		t.Parallel()

		p := newPendingTransactions()
		update := []byte(fmt.Sprintf("%s %s refs/heads/x\n", zero, old))
		p.prepared("git-1", forced("refs/heads/other"))
		p.prepared("git-1", update)

		require.False(t, p.committed(hash, "git-1", forced("refs/heads/other")), "the next transaction does not delete the reference")
		require.False(t, p.committed(hash, "git-1", update))
	})

	t.Run("aborted transactions are forgotten", func(t *testing.T) {
		t.Parallel()

		p := newPendingTransactions()
		p.prepared("git-1", packedPart)
		p.prepared("git-1", loosePart)
		p.aborted("git-1", packedPart)
		p.aborted("git-1", loosePart)

		require.Empty(t, p.invocations)
	})

	t.Run("without an invocation everything is recorded", func(t *testing.T) {
		t.Parallel()

		p := newPendingTransactions()
		p.prepared("", packedPart)
		p.prepared("", loosePart)

		require.False(t, p.committed(hash, "", packedPart))
		require.Empty(t, p.invocations)

		var nilPending *pendingTransactions
		nilPending.prepared("git-1", packedPart)
		nilPending.aborted("git-1", packedPart)
		require.False(t, nilPending.committed(hash, "git-1", packedPart))
	})

	t.Run("unseen transactions are recorded", func(t *testing.T) {
		t.Parallel()

		p := newPendingTransactions()
		require.False(t, p.committed(hash, "git-1", packedPart))
	})

	t.Run("stale processes expire", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		p := newPendingTransactions()
		p.now = func() time.Time { return now }

		p.prepared("dead", packedPart)
		p.prepared("dead", loosePart)

		now = now.Add(pendingTransactionsTTL + time.Second)
		p.prepared("alive", loosePart)

		require.Len(t, p.invocations, 1)
		require.Contains(t, p.invocations, "alive")
		require.False(t, p.committed(hash, "dead", packedPart), "an expired cleanup is recorded rather than lost")
	})

	t.Run("the number of processes is bounded", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		p := newPendingTransactions()
		p.now = func() time.Time { return now }

		for i := 0; i < maxPendingInvocations+10; i++ {
			now = now.Add(time.Millisecond)
			p.prepared(fmt.Sprintf("git-%d", i), loosePart)
		}

		require.Len(t, p.invocations, maxPendingInvocations)
		require.NotContains(t, p.invocations, "git-0", "the oldest process is evicted first")
		require.Contains(t, p.invocations, fmt.Sprintf("git-%d", maxPendingInvocations+9))
	})
}
