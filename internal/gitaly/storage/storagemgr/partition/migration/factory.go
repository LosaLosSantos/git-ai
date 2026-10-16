package migration

import (
	"context"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/keyvalue"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/storagemgr"
	"github.com/feder-cr/git-ai/internal/log"
)

// migrationFactory defines a partition factory that wraps another partition factory.
type migrationFactory struct {
	factory    storagemgr.PartitionFactory
	metrics    Metrics
	migrations *[]Migration
}

// NewFactory returns a new Factory.
//
// migrations is a list of configured migrations that must be performed on repositories.
func NewFactory(
	factory storagemgr.PartitionFactory,
	metrics Metrics,
	migrations *[]Migration,
) storagemgr.PartitionFactory {
	return &migrationFactory{
		factory:    factory,
		metrics:    metrics,
		migrations: migrations,
	}
}

// New returns a new Partition instance.
func (f migrationFactory) New(
	ctx context.Context,
	logger log.Logger,
	partitionID storage.PartitionID,
	db keyvalue.Transactioner,
	storageName string,
	storagePath string,
	absoluteStateDir string,
	stagingDir string,
	snapshotDir string,
) storagemgr.Partition {
	partition := f.factory.New(ctx, logger, partitionID, db, storageName, storagePath, absoluteStateDir, stagingDir, snapshotDir)
	return newPartition(partition, logger, f.metrics, storageName, f.migrations)
}
