// Package ancestry answers the ancestry questions of a landing: whether a commit is reachable from
// another one, and what their merge base is. Git answers them by running git merge-base, and the
// graph checker answers them from the stored graph index (see internal/graft/derived/graph). Both
// answer the same questions with the same results, so a landing gives the same outcome with either.
package ancestry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/graft/derived/graph"
	"github.com/feder-cr/git-ai/internal/log"
)

// graphKindName and graphKindVersion identify the graph entries in the derived-data store, as
// defined by internal/graft/derived/graph.
const (
	graphKindName    = graph.KindName
	graphKindVersion = graph.KindVersion
)

// Checker answers the ancestry questions of one landing attempt. A Checker is used by one
// goroutine at a time.
type Checker interface {
	// IsAncestor reports whether ancestor is reachable from descendant, including when both are
	// the same commit.
	IsAncestor(ctx context.Context, ancestor, descendant git.ObjectID) (bool, error)
	// MergeBase returns the merge base of a and b. When several best common ancestors exist, it
	// returns the one with the smallest object ID, so that Git and the graph checker agree. The
	// boolean is false when a and b have no common ancestor.
	MergeBase(ctx context.Context, a, b git.ObjectID) (git.ObjectID, bool, error)
}

// Git returns a Checker that runs git for each question. It is the behaviour without the graph
// index.
func Git(repo *localrepo.Repo) Checker {
	return gitChecker{repo: repo}
}

// Graph returns a Checker that answers from the graph index of repo. The index reads the parents
// of commits through reader and stores its entries in store, so that they are not computed again.
// A nil store computes the entries in memory only.
//
// Graph returns an error for repositories whose history git does not walk as the object database
// describes it: shallow repositories, repositories with grafts and repositories with replace refs.
// The answers of the index would differ from git for them.
func Graph(ctx context.Context, repo *localrepo.Repo, reader catfile.ObjectContentReader, store *derived.Store) (Checker, error) {
	if repo == nil || reader == nil {
		return nil, errors.New("graph ancestry needs a repository and an object reader")
	}

	repoPath, err := repo.Path(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting repository path: %w", err)
	}
	if err := checkHistoryWalkedAsStored(repoPath); err != nil {
		return nil, err
	}

	index := graph.NewIndex(graph.NewCatfileSource(reader), store.Kind(graphKindName, graphKindVersion))
	return graphChecker{index: index}, nil
}

// New returns the Graph checker when enabled, and the Git checker otherwise. When the graph
// checker cannot be constructed, New logs the reason and returns the Git checker.
func New(ctx context.Context, enabled bool, repo *localrepo.Repo, reader catfile.ObjectContentReader, store *derived.Store, logger log.Logger) Checker {
	if !enabled {
		return Git(repo)
	}

	checker, err := Graph(ctx, repo, reader, store)
	if err != nil {
		logger.WithError(err).Warn("graft: graph ancestry is unavailable, using git for ancestry")
		return Git(repo)
	}

	return checker
}

// checkHistoryWalkedAsStored returns an error when the repository at repoPath has shallow
// commits, grafts or replace refs, which change the history git walks.
func checkHistoryWalkedAsStored(repoPath string) error {
	for _, name := range []string{"shallow", filepath.Join("info", "grafts")} {
		if err := requireAbsent(filepath.Join(repoPath, name)); err != nil {
			return err
		}
	}

	replaced, err := os.ReadDir(filepath.Join(repoPath, "refs", "replace"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading replace refs: %w", err)
	}
	if len(replaced) > 0 {
		return errors.New("repository has replace refs")
	}

	packed, err := os.ReadFile(filepath.Join(repoPath, "packed-refs"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading packed refs: %w", err)
	}
	if bytes.Contains(packed, []byte(" refs/replace/")) {
		return errors.New("repository has replace refs")
	}

	return nil
}

// requireAbsent returns an error when path exists.
func requireAbsent(path string) error {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return fmt.Errorf("repository has %s", filepath.Base(path))
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("checking %s: %w", filepath.Base(path), err)
	}
}

// gitChecker answers the questions with git.
type gitChecker struct {
	repo *localrepo.Repo
}

func (c gitChecker) IsAncestor(ctx context.Context, ancestor, descendant git.ObjectID) (bool, error) {
	return c.repo.IsAncestor(ctx, ancestor.Revision(), descendant.Revision())
}

func (c gitChecker) MergeBase(ctx context.Context, a, b git.ObjectID) (git.ObjectID, bool, error) {
	var stdout, stderr bytes.Buffer
	if err := c.repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "merge-base",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--all"}},
			Args:  []string{a.String(), b.String()},
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		// git merge-base exits with status 1 when there is no common ancestor.
		if status, ok := command.ExitStatus(err); ok && status == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("merge-base: %w, stderr: %q", err, stderr.String())
	}

	var best string
	for _, line := range strings.Fields(stdout.String()) {
		if best == "" || line < best {
			best = line
		}
	}
	if best == "" {
		return "", false, nil
	}

	return git.ObjectID(best), true, nil
}

// graphChecker answers the questions with the graph index.
type graphChecker struct {
	index *graph.Index
}

func (c graphChecker) IsAncestor(ctx context.Context, ancestor, descendant git.ObjectID) (bool, error) {
	return c.index.IsAncestor(ctx, ancestor, descendant)
}

func (c graphChecker) MergeBase(ctx context.Context, a, b git.ObjectID) (git.ObjectID, bool, error) {
	base, err := c.index.MergeBase(ctx, a, b)
	if err != nil {
		return "", false, err
	}
	if base == "" {
		return "", false, nil
	}

	return base, true, nil
}
