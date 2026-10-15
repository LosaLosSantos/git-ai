package storage

import "context"

// NeedsSync indicates whether logic outside of the storage
// package should flush to disk. The writers need to flush if
// transactions are not enabled. If transactions are enabled,
// the flushing is responsibility of the transaction manager.
func NeedsSync(ctx context.Context) bool {
	return ExtractTransaction(ctx) == nil
}
