package mvcc_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/git/mvcc"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestIntegrationSnapshotReads wires up MVCC components together in order
// to test snapshotted reads. The steps are:
//
// 1. Create a MVCC repository with mvcc as ref backend
// 2. Create a commit in the repo
// 3. Prepare the mvcc cache with the latest manifest
// 4. Resolve the `refs/heads/main` ref: it should point to the first commit
// 5. Keep a copy of the current manifest hash
// 6. Create a second commit
// 7. Prepare the mvcc cache with the latest manifest
// 8. Resolve the `refs/heads/main` ref: it should point to the second commit
// 9. Prepare the mvcc cache with the manifest pointer pointing to the hash at step 5
// 10. Resolve the `refs/heads/main` ref: it should point to the FIRST commit (this is our snapshotted read)
func TestIntegrationSnapshotReads(t *testing.T) {
	ctx := testhelper.Context(t)

	cfg := testcfg.Build(t)
	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(cfg)
	catFileCache := catfile.NewCache(cfg)
	t.Cleanup(catFileCache.Stop)
	gitCmdFactory, cmdFactoryClose, err := gitcmd.NewExecCommandFactory(cfg, logger)
	require.NoError(t, err)
	t.Cleanup(cmdFactoryClose)

	// Create an MVCC repository
	repo, fullRepoPath := gittest.CreateMVCCRepository(t, ctx, cfg)

	// WriteCommit inspects the GPG signing feature flag, which panics in tests unless it is
	// explicitly set. This test does not exercise signing, so disable it.
	ctx = featureflag.ContextWithFeatureFlag(ctx, featureflag.GPGSigning, false)
	ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)

	// Localize repo on disk and create a local repo
	repoDir := cfg.Storages[0].Path
	relativePath, ok := strings.CutPrefix(fullRepoPath, repoDir)
	require.True(t, ok)
	localRepo := localrepo.New(logger, locator, gitCmdFactory, catFileCache, repo)

	// Create a LocalStorage instance
	rs := mvcc.NewLocalStorage(repoDir)

	// Create a first commit
	c1 := writeCommit(t, ctx, localRepo, "This is a README file", "README.md")

	// Simulate an RPC by creating a new Cache with a new Context
	cache, rpcCtx := prepareCache(t, rs, repoDir, relativePath)
	cache.Prepare(rpcCtx)
	require.NoError(t, cache.WaitUntilReady(rpcCtx))

	// Assert that resolving the main branch resolves to the first commit ID
	oid, err := localRepo.ResolveRevision(rpcCtx, "refs/heads/main")
	require.NoError(t, err)
	require.Equal(t, c1.String(), oid.String())

	// Keep this as a reference
	firstCommitManifestPointer := cache.ManifestHash()

	// Create another commit
	c2 := writeCommit(t, ctx, localRepo, "This is a README file with more stuff", "README.md")

	// Simulate another RPC by creating a new Cache with a new Context
	cache, rpcCtx = prepareCache(t, rs, repoDir, relativePath)
	cache.Prepare(rpcCtx)
	require.NoError(t, cache.WaitUntilReady(rpcCtx))

	// Assert that resolving the main branch resolves to the second commit ID
	oid, err = localRepo.ResolveRevision(rpcCtx, "refs/heads/main")
	require.NoError(t, err)
	require.Equal(t, c2.String(), oid.String())

	// Simulate a third RPC but this time we prepare the cache with the manifest
	// hash of the first commit. This is to validate that we can do snapshotted reads.
	cache, rpcCtx = prepareCache(t, rs, repoDir, relativePath)
	cache.PrepareWithManifest(rpcCtx, firstCommitManifestPointer)
	require.NoError(t, cache.WaitUntilReady(rpcCtx))

	// Assert that resolving the main branch now resolves to the first commit
	// because we are doing a snapshotted read.
	oid, err = localRepo.ResolveRevision(rpcCtx, "refs/heads/main")
	require.NoError(t, err)
	require.Equal(t, c1.String(), oid.String())
}

// TestIntegrationWrite wires up the MVCC components to exercise the write path:
//
//  1. Create an MVCC repository and record its base manifest pointer.
//  2. Prepare a mutator cache and write a commit through it. Git updates the per-RPC
//     temporary manifest pointer, not the canonical one, so the canonical pointer must
//     still be at the base at this point.
//  3. Commit the cache. This uploads the new artifacts and advances the canonical
//     manifest pointer with a compare-and-swap.
//  4. A fresh read must now resolve the newly written commit.
func TestIntegrationWrite(t *testing.T) {
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

	// The manifest pointer of the freshly created repository is our base.
	base, err := rs.ReadManifestPointer(ctx, relativePath)
	require.NoError(t, err)

	// Simulate a mutator RPC: build a mutator cache and carry it in the context so that Git
	// commands are invoked with GIT_MVCC_MANIFEST_PATH.
	cache := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, false)
	t.Cleanup(cache.Cleanup)
	rpcCtx := mvcc.ContextWithCache(ctx, cache)
	cache.Prepare(rpcCtx)
	require.NoError(t, cache.WaitUntilReady(rpcCtx))

	commitID := writeCommit(t, rpcCtx, localRepo, "This is a README file", "README.md")

	// The write went into the per-RPC temporary pointer, so the canonical pointer must not
	// have advanced yet.
	pointer, err := rs.ReadManifestPointer(ctx, relativePath)
	require.NoError(t, err)
	require.Equal(t, base, pointer)

	// Committing performs the compare-and-swap that advances the canonical pointer.
	require.NoError(t, cache.Commit(rpcCtx))

	pointer, err = rs.ReadManifestPointer(ctx, relativePath)
	require.NoError(t, err)
	require.NotEqual(t, base, pointer)

	// A fresh snapshot read now resolves the committed state.
	readCache, readCtx := prepareCache(t, rs, repoDir, relativePath)
	readCache.Prepare(readCtx)
	require.NoError(t, readCache.WaitUntilReady(readCtx))
	oid, err := localRepo.ResolveRevision(readCtx, "refs/heads/main")
	require.NoError(t, err)
	require.Equal(t, commitID.String(), oid.String())
}

// TestIntegrationConcurrentWriters models concurrent mutator RPCs that all resolve
// the same base manifest before any publishes. The first writer wins the compare-and-swap
// and advances the manifest pointer; the others built their updates on the now-stale base,
// so their commits must fail with a conflict (or the error registered via
// SetCommitConflictError) and leave the repository reflecting the first write.
func TestIntegrationConcurrentWriters(t *testing.T) {
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

	base, err := rs.ReadManifestPointer(ctx, relativePath)
	require.NoError(t, err)

	// Both writers resolve the same base manifest before either commits.
	newMutator := func() (*mvcc.LocalCache, context.Context) {
		cache := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, false)
		t.Cleanup(cache.Cleanup)
		cacheCtx := mvcc.ContextWithCache(ctx, cache)
		cache.Prepare(cacheCtx)
		require.NoError(t, cache.WaitUntilReady(cacheCtx))
		return cache, cacheCtx
	}

	writerA, ctxA := newMutator()
	writerB, ctxB := newMutator()

	// Writer C is built through a CacheFactory so its commit outcome is recorded in
	// the factory's metrics, which the assertion below inspects.
	factory := mvcc.NewCacheFactory(cfg.RuntimeDir, locator)
	t.Cleanup(func() { require.NoError(t, factory.Close(ctx)) })
	writerC, err := factory.NewCache(ctx, cfg.Storages[0].Name, relativePath, false)
	require.NoError(t, err)
	t.Cleanup(writerC.Cleanup)
	ctxC := mvcc.ContextWithCache(ctx, writerC)
	writerC.Prepare(ctxC)
	require.NoError(t, writerC.WaitUntilReady(ctxC))

	// Writer A publishes successfully, advancing the pointer off the base.
	commitA := writeCommit(t, ctxA, localRepo, "Written by writer A", "README.md")
	require.NoError(t, writerA.Commit(ctxA))

	afterA, err := rs.ReadManifestPointer(ctx, relativePath)
	require.NoError(t, err)
	require.NotEqual(t, base, afterA)

	// Writer B's Git commands operate on the base snapshot through its own temporary manifest
	// pointer, isolated from writer A's committed state. Its CAS against the stale base conflicts.
	writeCommit(t, ctxB, localRepo, "Written by writer B", "README.md")
	require.ErrorIs(t, writerB.Commit(ctxB), mvcc.ErrManifestPointerConflict)

	// The conflicting write left the canonical pointer untouched.
	afterB, err := rs.ReadManifestPointer(ctx, relativePath)
	require.NoError(t, err)
	require.Equal(t, afterA, afterB)

	// Writer C also built on the stale base, but registered the error its RPC should
	// surface on a conflict. Commit returns it wrapped around the original conflict
	// message, without ErrManifestPointerConflict in the chain so the registered error
	// is not rewritten by the mvcc middleware.
	conflictErr := errors.New("registered conflict error")
	writerC.SetCommitConflictError(conflictErr)
	writeCommit(t, ctxC, localRepo, "Written by writer C", "README.md")
	errC := writerC.Commit(ctxC)
	require.ErrorIs(t, errC, conflictErr)
	require.NotErrorIs(t, errC, mvcc.ErrManifestPointerConflict)
	require.EqualError(t, errC,
		fmt.Sprintf("registered conflict error: base manifest pointer changed: %v, base %q current %q",
			mvcc.ErrManifestPointerConflict, base.Hash, afterA.Hash))

	afterC, err := rs.ReadManifestPointer(ctx, relativePath)
	require.NoError(t, err)
	require.Equal(t, afterA, afterC)

	// The lost compare-and-swap is counted as a conflict even though Commit rewrote
	// it into the registered error, which no longer matches ErrManifestPointerConflict.
	require.NoError(t, testutil.CollectAndCompare(factory, strings.NewReader(`
# HELP gitaly_mvcc_commit_total Number of MVCC manifest-pointer commit attempts by outcome.
# TYPE gitaly_mvcc_commit_total counter
gitaly_mvcc_commit_total{status="conflict"} 1
`), "gitaly_mvcc_commit_total"))

	// A fresh snapshot read still reflects writer A's commit.
	readCache, readCtx := prepareCache(t, rs, repoDir, relativePath)
	readCache.Prepare(readCtx)
	require.NoError(t, readCache.WaitUntilReady(readCtx))
	oid, err := localRepo.ResolveRevision(readCtx, "refs/heads/main")
	require.NoError(t, err)
	require.Equal(t, commitA.String(), oid.String())
}

// prepareCache creates a Cache instance for the given repository and return the Cache and
// a context which contains the Cache and other values used during Git command execution.
func prepareCache(t *testing.T, rs mvcc.ObjectStorage, repoDir string, relativePath string) (*mvcc.LocalCache, context.Context) {
	t.Helper()

	// Simulate an RPC by creating a new Cache with a new Context
	cache := mvcc.NewLocalCache(rs, t.TempDir(), repoDir, relativePath, true)
	ctx := t.Context()
	ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)
	ctx = featureflag.ContextWithFeatureFlag(ctx, featureflag.GPGSigning, false)
	ctx = mvcc.ContextWithCache(ctx, cache)
	return cache, ctx
}

func writeCommit(t *testing.T, ctx context.Context, repo *localrepo.Repo, content string, filename string) git.ObjectID {
	t.Helper()

	// WriteCommit needs a tree object, not raw tree entries: write the file contents as a blob,
	// assemble a tree that references it, and persist that tree to obtain its OID. All of these
	// commands run through localrepo, which auto-detects the MVCC reference backend and selects the
	// matching Git binary.
	blobID, err := repo.WriteBlob(ctx, strings.NewReader(content), localrepo.WriteBlobConfig{})
	require.NoError(t, err)

	// Create tree
	tree := &localrepo.TreeEntry{
		Type: localrepo.Tree,
		Mode: "040000",
		Entries: []*localrepo.TreeEntry{
			{
				OID:  blobID,
				Mode: "100644",
				Path: filename,
				Type: localrepo.Blob,
			},
		},
	}
	require.NoError(t, tree.Write(ctx, repo))

	// Writing the commit with a reference makes the MVCC binary update the branch, which in turn
	// writes a new manifest for the repository.
	commitID, err := repo.WriteCommit(ctx, localrepo.WriteCommitConfig{
		Reference:      "refs/heads/main",
		AuthorName:     "Someone",
		AuthorEmail:    "someone@somewhere.com",
		CommitterName:  "Someone",
		CommitterEmail: "someone@somewhere.com",
		Message:        "Mvcc test commit",
		TreeID:         tree.OID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, commitID)

	return commitID
}
