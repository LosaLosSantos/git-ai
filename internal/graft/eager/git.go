package eager

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/log"
)

// RepoBuilder returns the repository in which the Git commands of the eager work
// run.
type RepoBuilder func(repo storage.Repository) *localrepo.Repo

// NewCommitsLister returns the NewCommitsFunc that runs git rev-list in the
// repository that build returns. It lists the commits of newOID that oldOID does
// not reach, at most MaxNewCommitsPerUpdate of them, and it lists the history of
// newOID when oldOID is zero.
func NewCommitsLister(build RepoBuilder) NewCommitsFunc {
	return func(ctx context.Context, repo storage.Repository, oldOID, newOID string) ([]git.ObjectID, error) {
		args := []string{newOID}
		if !changelog.IsZeroOID(oldOID) {
			args = append(args, "^"+oldOID)
		}

		var stdout, stderr bytes.Buffer
		if err := build(repo).ExecAndWait(ctx,
			gitcmd.Command{
				Name:  "rev-list",
				Flags: []gitcmd.Option{gitcmd.Flag{Name: fmt.Sprintf("--max-count=%d", MaxNewCommitsPerUpdate)}},
				Args:  args,
			},
			gitcmd.WithStdout(&stdout),
			gitcmd.WithStderr(&stderr),
		); err != nil {
			return nil, fmt.Errorf("rev-list: %w: %s", err, strings.TrimSpace(stderr.String()))
		}

		var commits []git.ObjectID
		for _, line := range strings.Split(stdout.String(), "\n") {
			if oid := strings.TrimSpace(line); oid != "" {
				commits = append(commits, git.ObjectID(oid))
			}
		}

		return commits, nil
	}
}

// IdleCommitGraphWriter writes the incremental commit-graph of a repository
// unless the repository's housekeeping runs, and reports whether it wrote. The
// housekeeping manager implements it, which serializes the write with
// OptimizeRepository of the same repository.
type IdleCommitGraphWriter interface {
	WriteCommitGraphIfIdle(ctx context.Context, repo *localrepo.Repo) (bool, error)
}

// CommitGraphWriterFor returns the CommitGraphWriter that runs the incremental
// commit-graph write of housekeeping through writer in the repository that
// build returns. When the repository is busy, because housekeeping runs or
// another Git process holds the commit-graph lock, the CommitGraphWriter returns
// an error that wraps ErrRepositoryBusy, so the Scheduler retries later.
func CommitGraphWriterFor(build RepoBuilder, writer IdleCommitGraphWriter) CommitGraphWriter {
	return func(ctx context.Context, repo storage.Repository) error {
		wrote, err := writer.WriteCommitGraphIfIdle(ctx, build(repo))
		if err != nil {
			if isLockContention(err) {
				return fmt.Errorf("%w: %w", ErrRepositoryBusy, err)
			}
			return err
		}

		if !wrote {
			return fmt.Errorf("%w: housekeeping is running", ErrRepositoryBusy)
		}

		return nil
	}
}

// isLockContention reports whether err is Git failing to take a lock file that
// another process holds, for example the commit-graph chain lock taken by a Git
// process that does not go through the housekeeping manager.
func isLockContention(err error) bool {
	return strings.Contains(err.Error(), ".lock': File exists")
}

// WarmEnvOpener returns the function that prepares the environment of the eager
// warmers for a repository: the repository, an object reader from cache and the
// derived-data store of the repository.
func WarmEnvOpener(build RepoBuilder, logger log.Logger, cache catfile.Cache) derived.OpenWarmEnvFunc {
	return func(ctx context.Context, repo storage.Repository) (derived.WarmEnv, func(), error) {
		lr := build(repo)

		reader, cancel, err := cache.ObjectReader(ctx, lr)
		if err != nil {
			return derived.WarmEnv{}, nil, fmt.Errorf("object reader: %w", err)
		}

		repoPath, err := lr.Path(ctx)
		if err != nil {
			cancel()
			return derived.WarmEnv{}, nil, fmt.Errorf("repository path: %w", err)
		}

		return derived.WarmEnv{
			Logger: logger,
			Repo:   lr,
			Reader: reader,
			Store:  derived.NewStore(repoPath),
		}, cancel, nil
	}
}
