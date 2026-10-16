package graph

import (
	"context"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
)

// catfileSource reads the parents of commits from the object database. Like the
// other derived kinds, it follows the parents stored in the objects, which is only
// the history git log shows when the repository is not a shallow clone.
type catfileSource struct {
	reader catfile.ObjectContentReader
}

// NewCatfileSource returns a Source that reads commits through reader.
func NewCatfileSource(reader catfile.ObjectContentReader) Source {
	return catfileSource{reader: reader}
}

// Parents returns the parents of the commit id. It returns catfile.NotFoundError
// when the commit does not exist.
func (s catfileSource) Parents(ctx context.Context, id git.ObjectID) ([]git.ObjectID, error) {
	commit, err := catfile.GetCommit(ctx, s.reader, id.Revision())
	if err != nil {
		return nil, fmt.Errorf("reading commit %s: %w", id, err)
	}

	parentIDs := commit.GetParentIds()
	parents := make([]git.ObjectID, 0, len(parentIDs))
	for _, parentID := range parentIDs {
		parents = append(parents, git.ObjectID(parentID))
	}

	return parents, nil
}
