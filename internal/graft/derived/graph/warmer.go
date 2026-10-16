package graph

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/derived"
)

func init() {
	derived.RegisterWarmer(kindName, warmCommits)
}

// warmCommits computes and stores the graph entries of commits and of their
// histories. Histories that were stored by earlier warms are not walked again.
func warmCommits(ctx context.Context, env derived.WarmEnv, commits []git.ObjectID) error {
	if env.Reader == nil {
		return errors.New("warming the graph kind needs an object reader")
	}

	ix := NewIndex(NewCatfileSource(env.Reader), env.Store.Kind(kindName, kindVersion))
	for _, commit := range commits {
		if _, err := ix.Generation(ctx, commit); err != nil {
			return fmt.Errorf("computing the graph entry of %s: %w", commit, err)
		}
	}

	return nil
}
