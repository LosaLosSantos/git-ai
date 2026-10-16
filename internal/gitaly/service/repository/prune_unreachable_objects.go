package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	housekeepingcfg "github.com/feder-cr/git-ai/internal/git/housekeeping/config"
	"github.com/feder-cr/git-ai/internal/git/stats"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// PruneUnreachableObjects prunes objects which aren't reachable from any of its references. To
// ensure that concurrently running commands do not reference those objects anymore when we execute
// the prune we enforce a grace-period: objects will only be pruned if they haven't been accessed
// for at least 30 minutes.
func (s *server) PruneUnreachableObjects(
	ctx context.Context,
	request *gitalypb.PruneUnreachableObjectsRequest,
) (*gitalypb.PruneUnreachableObjectsResponse, error) {
	repository := request.GetRepository()
	if err := s.locator.ValidateRepository(ctx, repository); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	repo := s.localRepoFactory.Build(repository)

	// Verify that the repository exists on-disk such that we can return a proper gRPC code in
	// case it doesn't.
	if _, err := repo.Path(ctx); err != nil {
		return nil, err
	}

	// MVCC repositories can only be optimized through geometric maintenance, which never drops
	// unreachable objects, and the full cruft repack that prune runs dies on MVCC. Refuse up front,
	// before any Git runs, rather than report a prune that didn't happen.
	refBackend, err := repo.ReferenceBackend(ctx)
	if err != nil {
		return nil, structerr.NewInternal("detecting reference backend: %w", err)
	}
	if refBackend.Name == git.ReferenceBackendMVCC.Name {
		return nil, structerr.NewFailedPrecondition("pruning unreachable objects is not supported on MVCC repositories")
	}

	// Verify that the repository is not an object pool. Pruning objects in object pools is not
	// a safe operation and is likely to cause corruption of object pool members.
	repoInfo, err := stats.RepositoryInfoForRepository(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("deriving repository info: %w", err)
	}
	if repoInfo.IsObjectPool {
		return nil, structerr.NewInvalidArgument("pruning objects for object pool")
	}

	if s.node != nil {
		if tx := storage.ExtractTransaction(ctx); tx != nil {
			tx.Repack(housekeepingcfg.RepackObjectsConfig{
				Strategy:            housekeepingcfg.RepackObjectsStrategyFullWithCruft,
				WriteMultiPackIndex: true,
				WriteBitmap:         len(repoInfo.Alternates.ObjectDirectories) == 0,
			})

			tx.WriteCommitGraphs(housekeepingcfg.WriteCommitGraphConfig{
				ReplaceChain: true,
			})

			commitLSN, err := tx.Commit(ctx)
			if err != nil {
				return nil, fmt.Errorf("commit: %w", err)
			}

			storage.LogTransactionCommit(ctx, s.logger, commitLSN, "prune unreachable objects")
		}

		return &gitalypb.PruneUnreachableObjectsResponse{}, nil
	}

	expireBefore := time.Now().Add(-30 * time.Minute)

	// We need to prune loose unreachable objects that exist in the repository.
	if err := housekeeping.PruneObjects(ctx, repo, housekeeping.PruneObjectsConfig{
		ExpireBefore: expireBefore,
	}); err != nil {
		return nil, structerr.NewInternal("pruning objects: %w", err)
	}

	// But we also have to prune unreachable objects part of cruft packs. The only way to do
	// that is to do a full repack. So unfortunately, this is quite expensive.
	if err := housekeeping.RepackObjects(ctx, repo, housekeepingcfg.RepackObjectsConfig{
		Strategy:            housekeepingcfg.RepackObjectsStrategyFullWithCruft,
		WriteMultiPackIndex: true,
		WriteBitmap:         len(repoInfo.Alternates.ObjectDirectories) == 0,
		CruftExpireBefore:   expireBefore,
	}); err != nil {
		return nil, structerr.NewInternal("repacking objects: %w", err)
	}

	// Rewrite the commit-graph so that it doesn't contain references to pruned commits
	// anymore.
	if err := housekeeping.WriteCommitGraph(ctx, repo, housekeepingcfg.WriteCommitGraphConfig{
		ReplaceChain: true,
	}); err != nil {
		return nil, structerr.NewInternal("rewriting commit-graph: %w", err)
	}

	stats.LogRepositoryInfo(ctx, s.logger, repo)

	return &gitalypb.PruneUnreachableObjectsResponse{}, nil
}
