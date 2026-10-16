package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	gitalycfgprom "github.com/feder-cr/git-ai/internal/gitaly/config/prometheus"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// mvccRepositorySetup is an MVCC repository with two manifests in its history:
// the canonical pointer is at currentHash, while olderHash names a valid,
// still-on-disk manifest from an earlier write. Tests reuse olderHash as a
// stand-in for "a new manifest produced by git housekeeping" so that no
// manifest needs to be serialized by hand.
type mvccRepositorySetup struct {
	repo        *localrepo.Repo
	pointerPath string
	olderHash   string
	currentHash string
}

func setupMVCCRepository(t *testing.T, ctx context.Context, cfg config.Cfg) mvccRepositorySetup {
	t.Helper()

	repoProto, repoPath := gittest.CreateRepository(t, gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC), cfg, gittest.CreateRepositoryConfig{
		ReferenceBackend:       git.ReferenceBackendMVCC,
		SkipCreationViaService: true,
	})

	writeCommit := func(content string) {
		gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithBranch("main"),
			gittest.WithTreeEntries(gittest.TreeEntry{
				Mode:    "100644",
				Path:    "README.md",
				Content: content,
			}),
		)
	}

	objectStorage := mvcc.NewLocalStorage(cfg.Storages[0].Path)

	writeCommit("first")
	olderPointer, err := objectStorage.ReadManifestPointer(ctx, repoProto.GetRelativePath())
	require.NoError(t, err)
	olderHash := olderPointer.Hash

	writeCommit("second")
	currentPointer, err := objectStorage.ReadManifestPointer(ctx, repoProto.GetRelativePath())
	require.NoError(t, err)
	currentHash := currentPointer.Hash
	require.NotEqual(t, olderHash, currentHash)

	return mvccRepositorySetup{
		repo:        localrepo.NewTestRepo(t, cfg, repoProto),
		pointerPath: filepath.Join(cfg.Storages[0].Path, repoProto.GetRelativePath(), "mvcc", "manifest"),
		olderHash:   olderHash,
		currentHash: currentHash,
	}
}

func readManifestPointer(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return strings.TrimSuffix(string(raw), "\n")
}

// TestOptimizeRepository_mvccCacheLifecycle verifies that background
// OptimizeRepository runs on MVCC repositories happen within an MVCC cache
// lifecycle: the base manifest is hydrated and ready before the optimization
// runs, and the resulting manifest pointer is committed afterwards.
func TestOptimizeRepository_mvccCacheLifecycle(t *testing.T) {
	gittest.SkipWithoutMVCCGit(t)
	t.Parallel()

	newManager := func(cfg config.Cfg) *RepositoryManager {
		return New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil,
			WithMVCCCacheSupport(mvcc.NewCacheFactory(cfg.RuntimeDir, config.NewLocator(cfg))))
	}

	requireLocalCache := func(t *testing.T, ctx context.Context) *mvcc.LocalCache {
		t.Helper()
		cache, ok := mvcc.CacheFromContext(ctx).(*mvcc.LocalCache)
		require.True(t, ok, "optimization must run within an MVCC cache lifecycle")
		return cache
	}

	t.Run("optimization without changes leaves the pointer unchanged", func(t *testing.T) {
		t.Parallel()
		ctx := testhelper.Context(t)
		cfg := testcfg.Build(t)
		setup := setupMVCCRepository(t, ctx, cfg)

		manager := newManager(cfg)
		var baseManifest string
		manager.optimizeFunc = func(ctx context.Context, _ *localrepo.Repo, _ housekeeping.OptimizationStrategy) error {
			cache := requireLocalCache(t, ctx)
			// The cache must be hydrated by the time the optimization runs, even if
			// the optimization itself never spawns a Git command that would wait for
			// readiness: the staged pointer file must exist and hold the base manifest.
			require.NotEmpty(t, cache.ManifestPath())
			baseManifest = readManifestPointer(t, cache.ManifestPath())
			return nil
		}

		require.NoError(t, manager.OptimizeRepository(ctx, setup.repo))
		require.Equal(t, setup.currentHash, baseManifest)
		require.Equal(t, setup.currentHash, readManifestPointer(t, setup.pointerPath))
	})

	t.Run("optimization producing a new manifest advances the pointer", func(t *testing.T) {
		t.Parallel()
		ctx := testhelper.Context(t)
		cfg := testcfg.Build(t)
		setup := setupMVCCRepository(t, ctx, cfg)

		manager := newManager(cfg)
		manager.optimizeFunc = func(ctx context.Context, _ *localrepo.Repo, _ housekeeping.OptimizationStrategy) error {
			// Simulate git housekeeping producing a new manifest by pointing the
			// staged pointer at the older, still-on-disk manifest.
			cache := requireLocalCache(t, ctx)
			return os.WriteFile(cache.ManifestPath(), []byte(setup.olderHash), 0o644)
		}

		require.NoError(t, manager.OptimizeRepository(ctx, setup.repo))
		require.Equal(t, setup.olderHash, readManifestPointer(t, setup.pointerPath))
	})

	t.Run("concurrently advanced pointer discards the optimization", func(t *testing.T) {
		t.Parallel()
		ctx := testhelper.Context(t)
		cfg := testcfg.Build(t)
		setup := setupMVCCRepository(t, ctx, cfg)

		manager := newManager(cfg)
		manager.optimizeFunc = func(ctx context.Context, _ *localrepo.Repo, _ housekeeping.OptimizationStrategy) error {
			cache := requireLocalCache(t, ctx)
			// A concurrent writer advances the canonical pointer mid-optimization.
			require.NoError(t, os.WriteFile(setup.pointerPath, []byte(setup.olderHash), 0o644))
			return os.WriteFile(cache.ManifestPath(), []byte(setup.olderHash), 0o644)
		}

		require.ErrorIs(t, manager.OptimizeRepository(ctx, setup.repo), mvcc.ErrManifestPointerConflict)
		// The concurrent write stands; the discarded optimization must not clobber it.
		require.Equal(t, setup.olderHash, readManifestPointer(t, setup.pointerPath))
	})

	t.Run("passthrough when a cache is already active on the context", func(t *testing.T) {
		t.Parallel()
		ctx := testhelper.Context(t)
		cfg := testcfg.Build(t)
		setup := setupMVCCRepository(t, ctx, cfg)

		// On the RPC path the mvcc middleware owns the cache lifecycle including
		// Commit. The manager must reuse the existing cache and not commit.
		injected := mvcc.NewLocalCache(mvcc.NewLocalStorage(cfg.Storages[0].Path),
			cfg.RuntimeDir, cfg.Storages[0].Path, setup.repo.GetRelativePath(), false)
		injected.Prepare(ctx)
		defer injected.Cleanup()
		ctx = mvcc.ContextWithCache(ctx, injected)

		manager := newManager(cfg)
		manager.optimizeFunc = func(ctx context.Context, _ *localrepo.Repo, _ housekeeping.OptimizationStrategy) error {
			require.Same(t, injected, mvcc.CacheFromContext(ctx))
			return nil
		}

		require.NoError(t, manager.OptimizeRepository(ctx, setup.repo))
		require.Equal(t, setup.currentHash, readManifestPointer(t, setup.pointerPath))
		require.NoError(t, injected.Commit(ctx))
	})
}

// TestOptimizeRepository_mvccPassthroughForNonMVCCRepository verifies that
// repositories not using the MVCC reference backend are optimized without a
// cache lifecycle even when MVCC cache support is configured.
func TestOptimizeRepository_mvccPassthroughForNonMVCCRepository(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repoProto, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	manager := New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil,
		WithMVCCCacheSupport(mvcc.NewCacheFactory(cfg.RuntimeDir, config.NewLocator(cfg))))

	called := false
	manager.optimizeFunc = func(ctx context.Context, _ *localrepo.Repo, _ housekeeping.OptimizationStrategy) error {
		called = true
		require.Nil(t, mvcc.CacheFromContext(ctx))
		return nil
	}

	require.NoError(t, manager.OptimizeRepository(ctx, repo))
	require.True(t, called, "the optimization must run for non-MVCC repositories")
}

func TestWithMVCCCacheSupport_wiring(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	factory := mvcc.NewCacheFactory(cfg.RuntimeDir, config.NewLocator(cfg))
	m := New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil,
		WithMVCCCacheSupport(factory))

	require.Same(t, factory, m.cacheFactory)
}

// TestWithMVCCCache_passthroughWhenUnconfigured verifies the safety guard: when
// MVCC cache support is not configured, withMVCCCache is a transparent
// passthrough (it just runs fn), so non-MVCC deployments optimize exactly as
// before and never touch the repo/backend detection.
func TestWithMVCCCache_passthroughWhenUnconfigured(t *testing.T) {
	t.Parallel()

	m := New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil)
	require.Nil(t, m.cacheFactory, "precondition: MVCC support not configured")

	called := false
	err := m.withMVCCCache(testhelper.Context(t), nil, func(context.Context) error {
		called = true
		return nil
	})

	require.NoError(t, err)
	require.True(t, called, "fn must run even when MVCC cache support is unconfigured")
}

// TestPackRefsIfNeeded_mvccSkipsReferenceRepacking verifies that reference
// repacking is skipped for MVCC repositories even when the strategy asks for it,
// so the skip does not silently depend on the repository statistics reporting no
// references for this backend.
func TestPackRefsIfNeeded_mvccSkipsReferenceRepacking(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	m := New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil)

	alwaysRepackReferences := mockOptimizationStrategy{
		shouldRepackReferences: func(context.Context) bool { return true },
	}

	t.Run("mvcc backend skips", func(t *testing.T) {
		setup := setupMVCCRepository(t, ctx, cfg)

		didPackRefs, err := m.packRefsIfNeeded(ctx, setup.repo, alwaysRepackReferences)
		require.NoError(t, err)
		require.False(t, didPackRefs)
	})

	t.Run("files backend still packs", func(t *testing.T) {
		repoProto, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
			ReferenceBackend:       git.ReferenceBackendFiles,
			SkipCreationViaService: true,
		})

		didPackRefs, err := m.packRefsIfNeeded(ctx, localrepo.NewTestRepo(t, cfg, repoProto), alwaysRepackReferences)
		require.NoError(t, err)
		require.True(t, didPackRefs)
	})
}

// TestOptimizeRepository_mvccRunsToCompletion exercises the real optimization
// path (no optimizeFunc seam) against an MVCC repository. The commit-graph
// heuristic used to reject the MVCC backend outright, which failed the whole run
// before the MVCC cache could ever be committed.
func TestOptimizeRepository_mvccRunsToCompletion(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	m := New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil,
		WithMVCCCacheSupport(mvcc.NewCacheFactory(cfg.RuntimeDir, config.NewLocator(cfg))))

	setup := setupMVCCRepository(t, ctx, cfg)

	require.NoError(t, m.OptimizeRepository(ctx, setup.repo))

	// Nothing the optimization does publishes a manifest yet: objects are not
	// MVCC-managed and reference repacking is skipped for this backend.
	require.Equal(t, setup.currentHash, readManifestPointer(t, setup.pointerPath))
}
