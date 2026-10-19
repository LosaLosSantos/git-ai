package storagecore

import (
	"context"
	"errors"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/checkpoint"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gc"
	"github.com/feder-cr/git-ai/internal/graft/corestore/maintenance"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// CreateCheckpoint implements gitalypb.StorageCoreServiceServer
// (doc/graft/PLAN7.md, "Checkpoint creation").
func (s *server) CreateCheckpoint(ctx context.Context, req *gitalypb.CreateCheckpointRequest) (*gitalypb.CreateCheckpointResponse, error) {
	repo := req.GetRepository()
	if err := s.validateRepository(ctx, repo, storage.WithSkipRepositoryExistenceCheck()); err != nil {
		return nil, err
	}
	core, err := s.enabledCore()
	if err != nil {
		return nil, err
	}

	cp := checkpoint.New(core, s.deps.GetLogger(), s.deps.GetLocator(), s.deps.GetRepositoryFactory(), s.deps.GetGraftStorageSyncer())
	created, isNew, err := cp.Create(ctx, repo)
	if err != nil {
		return nil, err
	}

	resp := &gitalypb.CreateCheckpointResponse{
		CheckpointSequence: created.Seq,
		Created:            isNew,
		PackBytes:          created.PackBytes,
	}
	if created.Seq == 0 {
		// No object to consolidate: there is no checkpoint.
		return resp, nil
	}
	refs, err := core.Meta.CheckpointRefs(ctx, created.RepoID, created.Seq)
	if err != nil {
		return nil, structerr.NewUnavailable("storage core: checkpoint references: %w", err)
	}
	resp.References = uint32(len(refs))
	return resp, nil
}

// CollectGarbage implements gitalypb.StorageCoreServiceServer
// (doc/graft/PLAN7.md, "Pack GC"). It holds the repository's maintenance lease
// while it collects, renewed (maintenance.HoldLease), so that it does not run
// alongside the background maintenance or another request; correctness does
// not depend on the lease.
func (s *server) CollectGarbage(ctx context.Context, req *gitalypb.CollectGarbageRequest) (*gitalypb.CollectGarbageResponse, error) {
	repo := req.GetRepository()
	if err := s.validateRepository(ctx, repo, storage.WithSkipRepositoryExistenceCheck()); err != nil {
		return nil, err
	}
	core, err := s.enabledCore()
	if err != nil {
		return nil, err
	}
	ttl, err := s.deps.GetCfg().Graft.Storage.LeaseTTLOrDefault()
	if err != nil {
		return nil, structerr.NewInternal("%w", err)
	}

	record, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return nil, structerr.NewNotFound("repository is not tracked by the storage core")
	}
	if err != nil {
		return nil, structerr.NewUnavailable("storage core: %w", err)
	}

	holder := maintenance.RequestLeaseHolder(core)
	acquired, err := core.Meta.AcquireMaintenanceLease(ctx, record.ID, holder, ttl)
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return nil, structerr.NewNotFound("repository is not tracked by the storage core")
	}
	if err != nil {
		return nil, structerr.NewUnavailable("storage core: maintenance lease: %w", err)
	}
	if !acquired {
		return nil, structerr.NewFailedPrecondition("another maintenance operation of this repository is in progress")
	}
	// The lease is renewed while the collection runs, like the background
	// maintenance's: a collection longer than lease_ttl keeps others out.
	lost := false
	leaseCtx, release := maintenance.HoldLease(ctx, core, s.deps.GetLogger(), record.ID, holder, ttl, func() { lost = true })

	report, err := gc.New(core, s.deps.GetLogger()).CollectRepository(leaseCtx, record.ID, req.GetDryRun())
	release()
	if err != nil {
		if lost {
			return nil, structerr.NewAborted("the maintenance lease was lost to another maintenance operation of this repository: %w", err)
		}
		return nil, err
	}
	return &gitalypb.CollectGarbageResponse{
		Boundary:           report.Boundary,
		CoveredDeleted:     uint32(report.CoveredDeleted),
		CheckpointsDropped: uint32(report.CheckpointsDropped),
		OrphansDeleted:     uint32(report.OrphansDeleted),
		OrphansObserved:    uint32(report.OrphansObserved),
		OrphansInGrace:     uint32(report.OrphansInGrace),
		PacksDeleted:       uint32(report.PacksDeleted),
		BytesReclaimed:     report.BytesReclaimed,
		DryRun:             report.DryRun,
	}, nil
}

// CollectRemovedRepositories implements gitalypb.StorageCoreServiceServer
// (doc/graft/PLAN7.md, "Removed repositories").
func (s *server) CollectRemovedRepositories(ctx context.Context, req *gitalypb.CollectRemovedRepositoriesRequest) (*gitalypb.CollectRemovedRepositoriesResponse, error) {
	storageName := req.GetStorageName()
	if _, err := s.deps.GetLocator().GetStorageByName(ctx, storageName); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}
	core, err := s.enabledCore()
	if err != nil {
		return nil, err
	}

	collector := gc.New(core, s.deps.GetLogger())
	reconciled, err := collector.Reconcile(ctx, req.GetDryRun())
	if err != nil {
		return nil, err
	}
	report, err := collector.CollectRemoved(ctx, storageName, req.GetDryRun())
	if err != nil {
		return nil, err
	}
	return &gitalypb.CollectRemovedRepositoriesResponse{
		Repositories:        uint32(report.RemovedRepos),
		PacksDeleted:        uint32(report.PacksDeleted),
		BytesReclaimed:      report.BytesReclaimed,
		DryRun:              report.DryRun,
		LeaksFound:          uint32(reconciled.LeaksFound),
		LeaksTombstoned:     uint32(reconciled.LeaksTombstoned),
		IdsAheadOfAllocator: uint32(reconciled.AheadOfAllocator),
	}, nil
}
