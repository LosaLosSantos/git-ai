// Package ancestry answers the ancestry questions of a landing: whether a commit is reachable from
// another one, what their merge base is, and which commits a change adds to a branch. Git answers
// them by running git merge-base and git rev-list, and the graph checker answers them from the
// stored graph index (see internal/graft/derived/graph). Both answer the same questions with the
// same results, so a landing gives the same outcome with either.
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
	// Range returns the commits reachable from head and not from base, as git rev-list
	// base..head lists them, each with all of its parents, in no particular order. It is empty
	// when head is reachable from base.
	Range(ctx context.Context, base, head git.ObjectID) ([]RangeCommit, error)
}

// RangeCommit is a commit of a range with all of its parents in order, as git rev-list --parents
// prints it.
type RangeCommit struct {
	ID      git.ObjectID
	Parents []git.ObjectID
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

	source := graph.NewCatfileSource(reader)
	index := graph.NewIndex(source, store.Kind(graphKindName, graphKindVersion))
	return graphChecker{index: index, source: source}, nil
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

// Range lists the range with one git rev-list --parents process.
func (c gitChecker) Range(ctx context.Context, base, head git.ObjectID) ([]RangeCommit, error) {
	objectHash, err := c.repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := c.repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "rev-list",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--parents"}},
			Args:  []string{base.String() + ".." + head.String()},
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return nil, fmt.Errorf("rev-list: %w, stderr: %q", err, stderr.String())
	}

	var commits []RangeCommit
	for _, line := range strings.Split(stdout.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		ids := make([]git.ObjectID, 0, len(fields))
		for _, field := range fields {
			id, err := objectHash.FromHex(field)
			if err != nil {
				return nil, fmt.Errorf("parsing rev-list output %q: %w", line, err)
			}
			ids = append(ids, id)
		}
		commits = append(commits, RangeCommit{ID: ids[0], Parents: ids[1:]})
	}

	return commits, nil
}

// graphChecker answers the questions with the graph index.
type graphChecker struct {
	index *graph.Index
	// source reads the parents of the commits of a range.
	source graph.Source
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

// Range walks down from head through the parents of the commits that the index does not find
// reachable from base. The ancestors of a commit reachable from base are all reachable from base, so
// the walk stops there without missing a commit of the range.
func (c graphChecker) Range(ctx context.Context, base, head git.ObjectID) ([]RangeCommit, error) {
	var commits []RangeCommit
	seen := map[git.ObjectID]bool{head: true}
	pending := []git.ObjectID{head}

	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]

		reachable, err := c.index.IsAncestor(ctx, id, base)
		if err != nil {
			return nil, err
		}
		if reachable {
			continue
		}

		parents, err := c.source.Parents(ctx, id)
		if err != nil {
			return nil, err
		}
		commits = append(commits, RangeCommit{ID: id, Parents: parents})

		for _, parent := range parents {
			if !seen[parent] {
				seen[parent] = true
				pending = append(pending, parent)
			}
		}
	}

	return commits, nil
}
