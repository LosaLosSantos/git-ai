package manager

import (
	"context"
	"fmt"

	"github.com/feder-cr/open_scion/internal/git/housekeeping"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
)

// WriteCommitGraphIfIdle writes an incremental commit-graph of the repository unless housekeeping
// or another such write runs on it. It returns false without writing when the repository is busy,
// in which case the caller should retry later. The write is serialized with OptimizeRepository:
// housekeeping that starts during the write waits for it to finish before optimizing, so the two
// never contend for the commit-graph chain lock.
//
// The write runs directly in the repository, outside of any transaction.
func (m *RepositoryManager) WriteCommitGraphIfIdle(ctx context.Context, repo *localrepo.Repo) (bool, error) {
	ok, cleanup := m.repositoryStates.tryRunningBackgroundWrite(repo)
	if !ok {
		return false, nil
	}
	defer cleanup()

	cfg, err := housekeeping.WriteCommitGraphConfigForRepository(ctx, repo)
	if err != nil {
		return true, fmt.Errorf("commit-graph config: %w", err)
	}

	return true, housekeeping.WriteCommitGraph(ctx, repo, cfg)
}
