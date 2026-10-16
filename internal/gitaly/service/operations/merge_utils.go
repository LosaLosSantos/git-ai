package operations

import (
	"context"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
)

// conflictingFileNames extracts the names of the conflicting files from a merge conflict error so
// that they can be attached to a MergeConflictError detail.
func conflictingFileNames(conflictErr *localrepo.MergeTreeConflictError) [][]byte {
	conflictingFiles := make([][]byte, 0, len(conflictErr.ConflictingFileInfo))
	for _, conflictingFileInfo := range conflictErr.ConflictingFileInfo {
		conflictingFiles = append(conflictingFiles, []byte(conflictingFileInfo.FileName))
	}

	return conflictingFiles
}

func (s *Server) merge(
	ctx context.Context,
	quarantineRepo *localrepo.Repo,
	author git.Signature,
	committer git.Signature,
	message string,
	ours string,
	theirs string,
	squash bool,
	sign bool,
) (string, error) {
	treeOID, err := quarantineRepo.MergeTree(ctx, ours, theirs, localrepo.WithAllowUnrelatedHistories(), localrepo.WithConflictingFileNamesOnly())
	if err != nil {
		return "", err
	}

	parents := []git.ObjectID{git.ObjectID(ours)}
	if !squash {
		parents = append(parents, git.ObjectID(theirs))
	}

	cfg := localrepo.WriteCommitConfig{
		TreeID:         treeOID,
		Message:        message,
		Parents:        parents,
		AuthorName:     author.Name,
		AuthorEmail:    author.Email,
		AuthorDate:     author.When,
		CommitterName:  committer.Name,
		CommitterEmail: committer.Email,
		CommitterDate:  committer.When,
		GitConfig:      s.gitConfig,
		Sign:           sign,
	}
	c, err := quarantineRepo.WriteCommit(
		ctx,
		cfg,
	)
	if err != nil {
		return "", fmt.Errorf("create commit from tree: %w", err)
	}

	return string(c), nil
}
