package transactions

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/praefect/config"
	"github.com/feder-cr/git-ai/internal/praefect/datastore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/transaction/voting"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// TestManager_VoteTransaction_CommittedConcludedLatch verifies that a transaction
// going through several voting phases only concludes its committed latch on the
// committed-phase vote — not on the preparing or prepared phases. This is the
// core of the fix for the race where PostReceiveHook was signaled after the
// prepared phase instead of after the write was actually committed.
func TestManager_VoteTransaction_CommittedConcludedLatch(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	const (
		storageName = "praefect-internal-1"
		relPath     = "repo/path"
		node        = "node-1"
	)

	isClosed := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}

	mgr := NewManager(config.Config{}, testhelper.NewLogger(t), &datastore.NoopWriteLockManager{})

	// A single voter with threshold 1 reaches quorum on its own vote, so each
	// VoteTransaction returns immediately and we can drive several subtransactions
	// (one per phase) sequentially without coordinating goroutines.
	tx, cleanup, err := mgr.RegisterTransaction(ctx, []Voter{{Name: node, Votes: 1}}, 1)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()

	vote := func(phase gitalypb.VoteTransactionRequest_Phase) {
		require.NoError(t, mgr.VoteTransaction(ctx, tx.ID(), storageName, relPath, node,
			phase, voting.VoteFromData([]byte("vote"))))
	}

	require.False(t, isClosed(tx.CommittedConcluded()),
		"latch must not be concluded before any vote")

	vote(gitalypb.VoteTransactionRequest_PREPARING_PHASE)
	require.False(t, isClosed(tx.CommittedConcluded()),
		"preparing-phase vote must not conclude the committed latch")

	vote(gitalypb.VoteTransactionRequest_PREPARED_PHASE)
	require.False(t, isClosed(tx.CommittedConcluded()),
		"prepared-phase vote must not conclude the committed latch")

	vote(gitalypb.VoteTransactionRequest_COMMITTED_PHASE)
	require.True(t, isClosed(tx.CommittedConcluded()),
		"committed-phase vote must conclude the committed latch")

	require.Equal(t, 3, tx.CountSubtransactions(),
		"each phase should have created its own subtransaction")
}

func TestManager_CancelTransactionNodeVoter(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	voters := []Voter{
		{Name: "1", Votes: 1},
		{Name: "2", Votes: 1},
		{Name: "3", Votes: 1},
	}
	threshold := uint(2)

	for _, tc := range []struct {
		desc      string
		register  bool
		node      string
		expErrMsg string
	}{
		{
			desc:      "No transaction exists",
			register:  false,
			node:      "1",
			expErrMsg: "transaction not found: 0",
		},
		{
			desc:      "Transaction exists",
			register:  true,
			node:      "1",
			expErrMsg: "",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			manager := NewManager(config.Config{}, testhelper.NewLogger(t), &datastore.NoopWriteLockManager{})

			var id uint64
			if tc.register {
				transaction, cleanup, err := manager.RegisterTransaction(ctx, voters, threshold)
				defer func() {
					err := cleanup()
					require.NoError(t, err)
				}()
				require.NoError(t, err)

				id = transaction.ID()
			}

			err := manager.CancelTransactionNodeVoter(id, "1")
			if tc.expErrMsg != "" {
				require.Error(t, err)
				require.Equal(t, tc.expErrMsg, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}
