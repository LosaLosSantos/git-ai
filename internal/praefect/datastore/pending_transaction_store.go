package datastore

import (
	"context"

	"github.com/feder-cr/open_scion/internal/praefect/datastore/glsql"
)

// PendingTransaction is a record of an in-flight mutating transaction that
// may not yet have had its generation incremented in the database. These
// records are written before Gitaly nodes are told to commit, so that a
// Praefect crash between quorum and the finalizer can be detected and
// recovered on startup.
type PendingTransaction struct {
	ID             int64
	RepositoryID   int64
	VirtualStorage string
	RelativePath   string
	ReplicaPath    string
	PrimaryStorage string
	ExpectedVoters []string
}

// PendingTransactionStore persists and retrieves pending transaction records.
type PendingTransactionStore interface {
	// RecordPendingTransaction writes a crash-recovery record before Gitaly nodes are
	// told to commit. The ID field of the input is ignored; the returned ID is the
	// newly created row's database-assigned primary key.
	RecordPendingTransaction(ctx context.Context, tx PendingTransaction) (int64, error)
	// DeletePendingTransaction removes the crash-recovery record once the finalizer has
	// successfully incremented the generation in the database.
	DeletePendingTransaction(ctx context.Context, id int64) error
	// GetPendingTransactions returns all pending records, ordered by creation
	// time ascending, for crash recovery on startup.
	GetPendingTransactions(ctx context.Context) ([]PendingTransaction, error)
}

// PostgresPendingTransactionStore is a PostgreSQL-backed PendingTransactionStore.
type PostgresPendingTransactionStore struct {
	db glsql.Querier
}

// NewPostgresPendingTransactionStore returns a new PostgresPendingTransactionStore.
func NewPostgresPendingTransactionStore(db glsql.Querier) *PostgresPendingTransactionStore {
	return &PostgresPendingTransactionStore{db: db}
}

// RecordPendingTransaction inserts a crash-recovery record into the pending_transactions
// table and returns the newly created row's ID.
func (s *PostgresPendingTransactionStore) RecordPendingTransaction(ctx context.Context, tx PendingTransaction) (int64, error) {
	const q = `
INSERT INTO pending_transactions
	(repository_id, virtual_storage, relative_path, replica_path, primary_storage, expected_voters)
VALUES
	($1, $2, $3, $4, $5, $6::text[])
RETURNING id
`
	var id int64
	if err := s.db.QueryRowContext(ctx, q,
		tx.RepositoryID, tx.VirtualStorage, tx.RelativePath, tx.ReplicaPath,
		tx.PrimaryStorage, tx.ExpectedVoters,
	).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// DeletePendingTransaction removes the crash-recovery record with the given ID
// from the pending_transactions table. It is a no-op if the ID does not exist.
func (s *PostgresPendingTransactionStore) DeletePendingTransaction(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_transactions WHERE id = $1`, id)
	return err
}

// GetPendingTransactions returns all rows from the pending_transactions table,
// ordered by creation time ascending. Returns an empty slice if the table is empty.
func (s *PostgresPendingTransactionStore) GetPendingTransactions(ctx context.Context) ([]PendingTransaction, error) {
	const q = `
SELECT id, repository_id, virtual_storage, relative_path, replica_path, primary_storage, expected_voters
FROM pending_transactions
ORDER BY created_at ASC
`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var txs []PendingTransaction
	for rows.Next() {
		var tx PendingTransaction
		var voters glsql.StringArray
		if err := rows.Scan(
			&tx.ID, &tx.RepositoryID, &tx.VirtualStorage, &tx.RelativePath,
			&tx.ReplicaPath, &tx.PrimaryStorage, &voters,
		); err != nil {
			return nil, err
		}
		tx.ExpectedVoters = voters.Slice()
		txs = append(txs, tx)
	}
	return txs, rows.Err()
}
