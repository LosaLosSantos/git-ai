package eager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	housekeepingmgr "github.com/feder-cr/git-ai/internal/git/housekeeping/manager"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	gitalycfgprom "github.com/feder-cr/git-ai/internal/gitaly/config/prometheus"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// createRepo creates an empty bare repository with the object format of the test
// configuration. It does not need a running server.
func createRepo(tb testing.TB, cfg config.Cfg) (*gitalypb.Repository, string) {
	tb.Helper()

	storagePath := cfg.Storages[0].Path
	repo := gittest.InitRepoDir(tb, storagePath, gittest.NewRepositoryName(tb))
	repoPath := filepath.Join(storagePath, repo.GetRelativePath())

	objectFormat := "sha1"
	if gittest.ObjectHashIsSHA256() {
		objectFormat = "sha256"
	}
	gittest.Exec(tb, cfg, "init", "--bare", "--quiet", "--object-format="+objectFormat, repoPath)

	return repo, repoPath
}

// testBuilder returns the RepoBuilder of a test repository setup.
func testBuilder(t *testing.T, cfg config.Cfg, cache catfile.Cache) RepoBuilder {
	t.Helper()

	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(cfg)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)

	return func(repo storage.Repository) *localrepo.Repo {
		return localrepo.New(logger, locator, gitCmdFactory, cache, repo)
	}
}

func TestNewCommitsLister(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)

	cache := catfile.NewCache(cfg)
	t.Cleanup(cache.Stop)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	middle := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))
	tip := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(middle))

	list := NewCommitsLister(testBuilder(t, cfg, cache))

	t.Run("new commits of an update", func(t *testing.T) {
		commits, err := list(ctx, repoProto, base.String(), tip.String())
		require.NoError(t, err)
		require.Equal(t, []git.ObjectID{tip, middle}, commits, "every new commit, newest first, without the old tip")
	})

	t.Run("creation lists the history", func(t *testing.T) {
		commits, err := list(ctx, repoProto, zeroOID, base.String())
		require.NoError(t, err)
		require.Equal(t, []git.ObjectID{base}, commits)
	})

	t.Run("nothing new", func(t *testing.T) {
		commits, err := list(ctx, repoProto, tip.String(), tip.String())
		require.NoError(t, err)
		require.Empty(t, commits)
	})

	t.Run("unknown commit", func(t *testing.T) {
		_, err := list(ctx, repoProto, oidA, tip.String())
		require.Error(t, err)
	})
}

func TestCommitGraphWriterFor(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)

	cache := catfile.NewCache(cfg)
	t.Cleanup(cache.Stop)

	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	write := CommitGraphWriterFor(testBuilder(t, cfg, cache), housekeepingmgr.New(gitalycfgprom.Config{}, testhelper.NewLogger(t), nil, nil))
	require.NoError(t, write(ctx, repoProto))

	require.FileExists(t, filepath.Join(repoPath, "objects", "info", "commit-graphs", "commit-graph-chain"))
}

// fakeIdleWriter is an IdleCommitGraphWriter with a fixed result.
type fakeIdleWriter struct {
	wrote bool
	err   error
}

func (w fakeIdleWriter) WriteCommitGraphIfIdle(context.Context, *localrepo.Repo) (bool, error) {
	return w.wrote, w.err
}

func TestCommitGraphWriterFor_Busy(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, _ := createRepo(t, cfg)

	cache := catfile.NewCache(cfg)
	t.Cleanup(cache.Stop)
	build := testBuilder(t, cfg, cache)

	t.Run("housekeeping running", func(t *testing.T) {
		err := CommitGraphWriterFor(build, fakeIdleWriter{wrote: false})(ctx, repoProto)
		require.ErrorIs(t, err, ErrRepositoryBusy)
	})

	t.Run("lock held by another Git process", func(t *testing.T) {
		lockErr := errors.New("writing commit-graph: exit status 128: fatal: Unable to create " +
			"'/repo/objects/info/commit-graphs/commit-graph-chain.lock': File exists.")

		err := CommitGraphWriterFor(build, fakeIdleWriter{wrote: true, err: lockErr})(ctx, repoProto)
		require.ErrorIs(t, err, ErrRepositoryBusy)
		require.ErrorIs(t, err, lockErr)
	})

	t.Run("other failure", func(t *testing.T) {
		otherErr := errors.New("writing commit-graph: exit status 128: fatal: bad object")

		err := CommitGraphWriterFor(build, fakeIdleWriter{wrote: true, err: otherErr})(ctx, repoProto)
		require.Equal(t, otherErr, err)
		require.NotErrorIs(t, err, ErrRepositoryBusy)
	})
}

// TestCommitGraphWriterFor_HeldChainLock checks that a real write that finds the
// commit-graph chain lock taken reports a busy repository.
func TestCommitGraphWriterFor_HeldChainLock(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)

	cache := catfile.NewCache(cfg)
	t.Cleanup(cache.Stop)

	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	write := CommitGraphWriterFor(testBuilder(t, cfg, cache), housekeepingmgr.New(gitalycfgprom.Config{}, testhelper.NewLogger(t), nil, nil))
	require.NoError(t, write(ctx, repoProto))

	// The commit differs from the first one, so the next write has a commit to add.
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("other"), gittest.WithMessage("other"))

	lockPath := filepath.Join(repoPath, "objects", "info", "commit-graphs", "commit-graph-chain.lock")
	require.NoError(t, os.WriteFile(lockPath, nil, mode.File))

	require.ErrorIs(t, write(ctx, repoProto), ErrRepositoryBusy)

	require.NoError(t, os.Remove(lockPath))
	require.NoError(t, write(ctx, repoProto))
}

func TestWarmEnvOpener(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, _ := createRepo(t, cfg)

	cache := catfile.NewCache(cfg)
	t.Cleanup(cache.Stop)

	open := WarmEnvOpener(testBuilder(t, cfg, cache), testhelper.NewLogger(t), cache)

	env, cleanup, err := open(ctx, repoProto)
	require.NoError(t, err)
	require.NotNil(t, env.Repo)
	require.NotNil(t, env.Reader)
	require.NotNil(t, env.Store)
	require.NotNil(t, env.Logger)
	cleanup()
}
