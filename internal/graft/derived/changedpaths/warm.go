package changedpaths

import (
	"context"
	"fmt"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/graft/derived"
)

// kindName and kindVersion identify the changed-paths kind in the derived-data
// store. Bump the version when the encoding or the computation changes.
const (
	kindName    = "changed-paths"
	kindVersion = 1
)

func init() {
	derived.RegisterWarmer(kindName, Warm)
}

// kindStore adapts a derived.KindStore, keyed by commit, to Store.
type kindStore struct {
	kind derived.KindStore
}

func (s kindStore) Get(commit string) ([]byte, bool, error) {
	return s.kind.Get(git.ObjectID(commit))
}

func (s kindStore) Put(commit string, data []byte) error {
	return s.kind.Put(git.ObjectID(commit), data)
}

// NewIndexFor returns the index of a repository whose objects are read through
// reader and whose summaries are persisted in store. A nil store persists nothing:
// queries then diff the path alone.
func NewIndexFor(reader catfile.ObjectContentReader, store *derived.Store) *Index {
	var persisted Store
	if store != nil {
		persisted = kindStore{kind: store.Kind(kindName, kindVersion)}
	}
	return NewIndex(NewObjectReader(reader), persisted)
}

// Warm computes and stores the summaries of the given commits. It is the eager
// warmer of the kind. Summaries that are already stored are not computed again.
func Warm(ctx context.Context, env derived.WarmEnv, commits []git.ObjectID) error {
	index := NewIndexFor(env.Reader, env.Store)
	for _, commit := range commits {
		if _, err := index.CommitSummary(ctx, commit.String()); err != nil {
			return fmt.Errorf("changed paths of %s: %w", commit, err)
		}
	}
	return nil
}
