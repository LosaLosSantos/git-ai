package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/structerr"
)

const (
	// WorktreesPrefix is a subdirectory in the repository where Gitaly creates worktrees.
	WorktreesPrefix = "worktrees"
	// GitlabWorktreePrefix is a subdirectory in the repository where Gitaly creates worktrees.
	GitlabWorktreePrefix = "gitlab-worktree"
)

// CleanupWorktrees cleans up stale and disconnected worktrees for the given repository.
func CleanupWorktrees(ctx context.Context, repo *localrepo.Repo) error {
	if _, err := repo.Path(ctx); err != nil {
		return err
	}

	worktreeThreshold := time.Now().Add(-6 * time.Hour)
	if err := cleanStaleWorktrees(ctx, repo, worktreeThreshold); err != nil {
		return structerr.NewInternal("cleanStaleWorktrees: %w", err)
	}

	if err := cleanDisconnectedWorktrees(ctx, repo, worktreeThreshold); err != nil {
		return structerr.NewInternal("cleanDisconnectedWorktrees: %w", err)
	}

	return nil
}

func maybePruneWorktreeDirectory(root string, entry fs.DirEntry, threshold time.Time) error {
	if !entry.IsDir() || (entry.Type()&fs.ModeSymlink != 0) {
		return nil
	}

	info, err := entry.Info()
	if err != nil {
		// It's fine if the entry has disappeared meanwhile, we wanted to remove it
		// anyway.
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("statting worktree: %w", err)
	}

	if info.ModTime().Before(threshold) {
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("worktree remove dir: %w", err)
		}
	}

	return nil
}

func cleanWorktreeDirectory(ctx context.Context, repo *localrepo.Repo, prefix string, threshold time.Time) error {
	repoPath, err := repo.Path(ctx)
	if err != nil {
		return err
	}

	worktreePath := filepath.Join(repoPath, prefix)
	worktreeEntries, err := os.ReadDir(worktreePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	for _, entry := range worktreeEntries {
		if err := maybePruneWorktreeDirectory(worktreePath, entry, threshold); err != nil {
			return err
		}
	}

	return nil
}

func cleanStaleWorktrees(ctx context.Context, repo *localrepo.Repo, threshold time.Time) error {
	return cleanWorktreeDirectory(ctx, repo, GitlabWorktreePrefix, threshold)
}

func cleanDisconnectedWorktrees(ctx context.Context, repo *localrepo.Repo, threshold time.Time) error {
	return cleanWorktreeDirectory(ctx, repo, WorktreesPrefix, threshold)
}
