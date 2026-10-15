package datastore

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testdb"
	"github.com/stretchr/testify/require"
)

func TestPostgresPendingTransactionStore(t *testing.T) {
	t.Parallel()

	db := testdb.New(t)
	ctx := testhelper.Context(t)

	newStore := func(t *testing.T) *PostgresPendingTransactionStore {
		t.Helper()
		db.TruncateAll(t)
		return NewPostgresPendingTransactionStore(db)
	}

	// seedRepo inserts a minimal repository row so the FK on pending_transactions is satisfied.
	seedRepo := func(t *testing.T, repositoryID int64, virtualStorage, relativePath string) {
		t.Helper()
		rs := NewPostgresRepositoryStore(db, nil)
		require.NoError(t, rs.CreateRepository(ctx, repositoryID, virtualStorage, relativePath, relativePath, "primary-1", nil, nil, false, false))
	}

	t.Run("empty table returns no transactions", func(t *testing.T) {
		s := newStore(t)
		txs, err := s.GetPendingTransactions(ctx)
		require.NoError(t, err)
		require.Empty(t, txs)
	})

	t.Run("record and retrieve", func(t *testing.T) {
		s := newStore(t)
		seedRepo(t, 42, "vs-1", "rel/path")

		id, err := s.RecordPendingTransaction(ctx, PendingTransaction{
			RepositoryID:   42,
			VirtualStorage: "vs-1",
			RelativePath:   "rel/path",
			ReplicaPath:    "replica/path",
			PrimaryStorage: "primary-1",
			ExpectedVoters: []string{"primary-1", "secondary-1", "secondary-2"},
		})
		require.NoError(t, err)
		require.NotZero(t, id)

		txs, err := s.GetPendingTransactions(ctx)
		require.NoError(t, err)
		require.Len(t, txs, 1)

		got := txs[0]
		require.Equal(t, id, got.ID)
		require.Equal(t, int64(42), got.RepositoryID)
		require.Equal(t, "vs-1", got.VirtualStorage)
		require.Equal(t, "rel/path", got.RelativePath)
		require.Equal(t, "replica/path", got.ReplicaPath)
		require.Equal(t, "primary-1", got.PrimaryStorage)
		require.ElementsMatch(t, []string{"primary-1", "secondary-1", "secondary-2"}, got.ExpectedVoters)
	})

	t.Run("multiple records returned oldest first", func(t *testing.T) {
		s := newStore(t)
		seedRepo(t, 1, "vs", "a")
		seedRepo(t, 2, "vs", "b")

		id1, err := s.RecordPendingTransaction(ctx, PendingTransaction{
			RepositoryID: 1, VirtualStorage: "vs", RelativePath: "a", ReplicaPath: "a",
			PrimaryStorage: "p", ExpectedVoters: []string{"p"},
		})
		require.NoError(t, err)
		id2, err := s.RecordPendingTransaction(ctx, PendingTransaction{
			RepositoryID: 2, VirtualStorage: "vs", RelativePath: "b", ReplicaPath: "b",
			PrimaryStorage: "p", ExpectedVoters: []string{"p"},
		})
		require.NoError(t, err)

		txs, err := s.GetPendingTransactions(ctx)
		require.NoError(t, err)
		require.Len(t, txs, 2)
		require.Equal(t, id1, txs[0].ID)
		require.Equal(t, id2, txs[1].ID)
	})

	t.Run("delete removes the record", func(t *testing.T) {
		s := newStore(t)
		seedRepo(t, 1, "vs", "r")

		id, err := s.RecordPendingTransaction(ctx, PendingTransaction{
			RepositoryID: 1, VirtualStorage: "vs", RelativePath: "r", ReplicaPath: "r",
			PrimaryStorage: "p", ExpectedVoters: []string{"p"},
		})
		require.NoError(t, err)

		require.NoError(t, s.DeletePendingTransaction(ctx, id))

		txs, err := s.GetPendingTransactions(ctx)
		require.NoError(t, err)
		require.Empty(t, txs)
	})

	t.Run("delete is a no-op for non-existent id", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.DeletePendingTransaction(ctx, 99999))
	})
}
