package mvcc_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// TestIntegrationAfterCommit checks that the functions registered with AfterCommit run only once
// the changes of the RPC are published, in registration order, and never for an RPC whose
// changes are thrown away because its commit lost the compare-and-swap or it was cleaned up
// without committing.
func TestIntegrationAfterCommit(t *testing.T) {
	ctx := testhelper.Context(t)

	cfg := testcfg.Build(t)
	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(cfg)
	catFileCache := catfile.NewCache(cfg)
	t.Cleanup(catFileCache.Stop)
	gitCmdFactory, cmdFactoryClose, err := gitcmd.NewExecCommandFactory(cfg, logger)
	require.NoError(t, err)
	t.Cleanup(cmdFactoryClose)

	repo, fullRepoPath := gittest.CreateMVCCRepository(t, ctx, cfg)

	ctx = featureflag.ContextWithFeatureFlag(ctx, featureflag.GPGSigning, false)
	ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)

	repoDir := cfg.Storages[0].Path
	relativePath, ok := strings.CutPrefix(fullRepoPath, repoDir)
	require.True(t, ok)
	localRepo := localrepo.New(logger, locator, gitCmdFactory, catFileCache, repo)
	rs := mvcc.NewLocalStorage(repoDir)

	newCache := func(readOnly bool) (*mvcc.LocalCache, context.Context) {
		cache := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, readOnly)
		t.Cleanup(cache.Cleanup)
		cacheCtx := mvcc.ContextWithCache(ctx, cache)
		cache.Prepare(cacheCtx)
		require.NoError(t, cache.WaitUntilReady(cacheCtx))
		return cache, cacheCtx
	}

	var mu sync.Mutex
	var ran []string
	record := func(name string) func() {
		return func() {
			mu.Lock()
			defer mu.Unlock()
			ran = append(ran, name)
		}
	}
	requireRan := func(t *testing.T, expected ...string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		require.Equal(t, expected, ran)
		ran = nil
	}

	t.Run("read-only caches register nothing", func(t *testing.T) {
		reader, _ := newCache(true)
		require.False(t, reader.AfterCommit(record("reader")))
		require.Empty(t, mvcc.CommitHandle(reader))
		require.NoError(t, reader.Commit(ctx))
		requireRan(t)
	})

	t.Run("the winner runs its functions, the loser does not", func(t *testing.T) {
		// Both writers resolve the same base manifest before either commits.
		winner, winnerCtx := newCache(false)
		loser, loserCtx := newCache(false)

		// Functions are found and registered through the handle, as hook requests do.
		handle := mvcc.CommitHandle(winner)
		require.NotEmpty(t, handle)
		require.Equal(t, handle, mvcc.CommitHandle(winner))
		require.Same(t, winner, mvcc.CacheByCommitHandle(handle))
		require.NotEqual(t, handle, mvcc.CommitHandle(loser))

		require.True(t, mvcc.CacheByCommitHandle(handle).AfterCommit(record("winner 1")))
		require.True(t, winner.AfterCommit(record("winner 2")))
		require.True(t, loser.AfterCommit(record("loser")))

		writeCommit(t, winnerCtx, localRepo, "Written by the winner", "README.md")
		writeCommit(t, loserCtx, localRepo, "Written by the loser", "README.md")

		// Nothing runs before the changes are published.
		requireRan(t)

		require.NoError(t, winner.Commit(winnerCtx))
		requireRan(t, "winner 1", "winner 2")

		// A committed cache can no longer be found or register anything.
		require.Nil(t, mvcc.CacheByCommitHandle(handle))
		require.False(t, winner.AfterCommit(record("winner late")))

		require.ErrorIs(t, loser.Commit(loserCtx), mvcc.ErrManifestPointerConflict)
		requireRan(t)
	})

	t.Run("a cache cleaned up without committing runs nothing", func(t *testing.T) {
		writer, writerCtx := newCache(false)
		handle := mvcc.CommitHandle(writer)

		require.True(t, writer.AfterCommit(record("writer")))
		writeCommit(t, writerCtx, localRepo, "Thrown away", "README.md")

		writer.Cleanup()
		requireRan(t)
		require.Nil(t, mvcc.CacheByCommitHandle(handle))
		require.False(t, writer.AfterCommit(record("writer late")))
	})

	t.Run("a commit without changes runs its functions", func(t *testing.T) {
		writer, writerCtx := newCache(false)

		require.True(t, writer.AfterCommit(record("unchanged")))
		require.NoError(t, writer.Commit(writerCtx))
		requireRan(t, "unchanged")
	})

	t.Run("an unknown handle finds nothing", func(t *testing.T) {
		require.Nil(t, mvcc.CacheByCommitHandle(""))
		require.Nil(t, mvcc.CacheByCommitHandle("unknown"))
	})
}
