package praefect

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/praefect/datastore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc"
)

// RecoverPendingTransactions runs on Praefect startup to reconcile any
// transactions that reached quorum but whose generation increment was never
// persisted to the database due to a crash.
//
// For each pending record it:
//  1. Queries every expected voter via CalculateChecksum to discover the
//     actual on-disk state.
//  2. Treats the primary's checksum as authoritative and marks secondaries
//     that match it as updated; others as outdated.
//  3. Calls IncrementGeneration for the updated set.
//  4. Queues replication jobs for the outdated set.
//  5. Deletes the pending record.
func RecoverPendingTransactions(
	ctx context.Context,
	pts datastore.PendingTransactionStore,
	rs datastore.RepositoryStore,
	queue datastore.ReplicationEventQueue,
	conns Connections,
	logger log.Logger,
) error {
	pending, err := pts.GetPendingTransactions(ctx)
	if err != nil {
		return fmt.Errorf("get pending transactions: %w", err)
	}

	if len(pending) == 0 {
		return nil
	}

	logger.WithField("count", len(pending)).Warn("recovering pending transactions from before crash")

	for _, tx := range pending {
		if err := recoverPendingTransaction(ctx, tx, pts, rs, queue, conns, logger); err != nil {
			// Log and continue: a failed recovery leaves the record in place so
			// the next startup can retry, while still recovering other repos.
			logger.WithError(err).WithField("repository_id", tx.RepositoryID).
				Warn("failed to recover pending transaction, will retry on next startup")
		}
	}
	return nil
}

func recoverPendingTransaction(
	ctx context.Context,
	tx datastore.PendingTransaction,
	pts datastore.PendingTransactionStore,
	rs datastore.RepositoryStore,
	queue datastore.ReplicationEventQueue,
	conns Connections,
	logger log.Logger,
) error {
	vsConns, ok := conns[tx.VirtualStorage]
	if !ok {
		return fmt.Errorf("no connections for virtual storage %q", tx.VirtualStorage)
	}

	primaryConn, ok := vsConns[tx.PrimaryStorage]
	if !ok {
		return fmt.Errorf("no connection for primary storage %q", tx.PrimaryStorage)
	}

	primaryChecksum, err := repoChecksum(ctx, primaryConn, tx.PrimaryStorage, tx.ReplicaPath)
	if err != nil {
		return fmt.Errorf("checksum from primary %q: %w", tx.PrimaryStorage, err)
	}

	var updated, outdated []string
	for _, voter := range tx.ExpectedVoters {
		if voter == tx.PrimaryStorage {
			continue
		}

		conn, ok := vsConns[voter]
		if !ok {
			logger.WithField("storage", voter).Warn("no connection for expected voter during recovery, treating as outdated")
			outdated = append(outdated, voter)
			continue
		}

		checksum, err := repoChecksum(ctx, conn, voter, tx.ReplicaPath)
		if err != nil {
			logger.WithError(err).WithField("storage", voter).Warn("checksum failed for voter during recovery, treating as outdated")
			outdated = append(outdated, voter)
			continue
		}

		if checksum == primaryChecksum {
			updated = append(updated, voter)
		} else {
			outdated = append(outdated, voter)
		}
	}

	// By advancing the generation here without a mechanism to check if
	// it was already advanced, for example due to a race, can cause it
	// to be over-advanced. But this is known and since consistent nodes
	// advance together, reads remain correct. And all out-of-date nodes
	// would also get the same generation during replication. Therefore,
	// it is safe to always increment the generation here.
	if err := rs.IncrementGeneration(ctx, tx.RepositoryID, tx.PrimaryStorage, updated); err != nil {
		if !errors.Is(err, datastore.ErrWriteToOutdatedNodes) {
			return fmt.Errorf("increment generation: %w", err)
		}
	}

	// The critical fix (IncrementGeneration) is done and the DB generation
	// is correct, which means reads will no longer be served from stale replicas.
	// We also enqueue ReplicationEvent for the outdated nodes below but even if
	// those explicit replication jobs fail, regular mutator RPCs would trigger
	// replication and eventually bring them up to date.
	// Therefore, it is safe to delete the pending transaction here.
	if err := pts.DeletePendingTransaction(ctx, tx.ID); err != nil {
		return fmt.Errorf("delete pending transaction: %w", err)
	}

	// Queue replication from primary to any secondaries whose state diverged.
	for _, secondary := range outdated {
		event := datastore.ReplicationEvent{
			Job: datastore.ReplicationJob{
				RepositoryID:      tx.RepositoryID,
				Change:            datastore.UpdateRepo,
				RelativePath:      tx.RelativePath,
				VirtualStorage:    tx.VirtualStorage,
				SourceNodeStorage: tx.PrimaryStorage,
				TargetNodeStorage: secondary,
			},
		}
		if _, err := queue.Enqueue(ctx, event); err != nil {
			if errors.As(err, &datastore.ReplicationEventExistsError{}) {
				continue
			}
			logger.WithError(err).WithField("secondary", secondary).
				Warn("failed to enqueue replication job during recovery")
		}
	}

	return nil
}

// repoChecksum calls CalculateChecksum on the given Gitaly node and returns
// the resulting checksum string.
func repoChecksum(ctx context.Context, conn *grpc.ClientConn, storage, replicaPath string) (string, error) {
	resp, err := gitalypb.NewRepositoryServiceClient(conn).CalculateChecksum(ctx,
		&gitalypb.CalculateChecksumRequest{
			Repository: &gitalypb.Repository{
				StorageName:  storage,
				RelativePath: replicaPath,
			},
		},
	)
	if err != nil {
		return "", err
	}
	return resp.GetChecksum(), nil
}
