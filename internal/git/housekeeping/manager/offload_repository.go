package manager

import (
	"context"
	"fmt"

	"github.com/feder-cr/open_scion/internal/git/housekeeping/config"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
)

// OffloadRepository tells a transaction that this repository needs to be offloaded with the specified configuration.
func (m *RepositoryManager) OffloadRepository(ctx context.Context, repo *localrepo.Repo, cfg config.OffloadingConfig) error {
	if m.node == nil {
		return fmt.Errorf("unable to retrieve storage node")
	}

	return m.runInTransaction(ctx, "housekeeping/offload", false, repo, func(ctx context.Context, tx storage.Transaction, repo *localrepo.Repo) error {
		tx.SetOffloadingConfig(cfg)
		return nil
	})
}
