package hook

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/mvcc"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/transaction"
	"github.com/feder-cr/open_scion/internal/gitlab"
	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/grpc/backchannel"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// TestReferenceTransactionHook_changesetsOfMVCCSnapshots checks that the committed updates of a
// request on a repository that uses the MVCC reference backend are only recorded once the request
// published its snapshot by committing its MVCC cache, and never when its changes are thrown away.
// The hook receives the request's cache through the hooks payload, as it does when Git runs it
// from another process.
func TestReferenceTransactionHook_changesetsOfMVCCSnapshots(t *testing.T) {
	t.Parallel()

	testhelper.SkipWithReftable(t, "MVCC backend selection is overridden by GIT_DEFAULT_REF_FORMAT")
	gittest.SkipWithoutMVCCGit(t)

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repo, repoPath := gittest.CreateMVCCRepository(t, ctx, cfg)
	repoDir := cfg.Storages[0].Path
	relativePath, ok := strings.CutPrefix(repoPath, repoDir)
	require.True(t, ok)
	rs := mvcc.NewLocalStorage(repoDir)

	hookManager := NewManager(
		cfg,
		config.NewLocator(cfg),
		testhelper.SharedLogger(t),
		gittest.NewCommandFactory(t, cfg),
		nil,
		transaction.NewManager(cfg, testhelper.SharedLogger(t), backchannel.NewRegistry()),
		gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
		NewPostReceiveRegistry(),
	)

	newCache := func(t *testing.T) (*mvcc.LocalCache, context.Context) {
		cache := mvcc.NewLocalCache(rs, cfg.RuntimeDir, repoDir, relativePath, false)
		t.Cleanup(cache.Cleanup)
		cacheCtx := mvcc.ContextWithCache(ctx, cache)
		cache.Prepare(cacheCtx)
		require.NoError(t, cache.WaitUntilReady(cacheCtx))
		return cache, cacheCtx
	}

	// commitUpdate runs the committed reference-transaction hook for an update of the given
	// reference with a hooks payload built in payloadCtx. The hook itself runs without the
	// request's cache in its context, like a hook request sent by Git.
	commitUpdate := func(t *testing.T, payloadCtx context.Context, reference string) {
		env, err := gitcmd.NewHooksPayload(
			payloadCtx,
			cfg,
			repo,
			gittest.DefaultObjectHash,
			nil,
			nil,
			gitcmd.ReferenceTransactionHook,
			nil,
		).Env()
		require.NoError(t, err)

		changes := fmt.Sprintf("%s %s %s\n", gittest.DefaultObjectHash.ZeroOID, gittest.DefaultObjectHash.EmptyTreeOID, reference)
		require.NoError(t, hookManager.ReferenceTransactionHook(ctx, ReferenceTransactionCommitted, []string{env}, strings.NewReader(changes)))
	}

	recorded := func(t *testing.T) []string {
		entries, err := changelog.List(repoPath, 0, 1000)
		require.NoError(t, err)

		var references []string
		for _, entry := range entries {
			references = append(references, entry.Reference)
		}
		return references
	}

	// The subtests share the log of the repository, so they run one after another.
	t.Run("recorded once the cache commits", func(t *testing.T) {
		cache, cacheCtx := newCache(t)

		commitUpdate(t, cacheCtx, "refs/heads/first")
		commitUpdate(t, cacheCtx, "refs/heads/second")
		require.Empty(t, recorded(t))

		require.NoError(t, cache.Commit(cacheCtx))
		require.Equal(t, []string{"refs/heads/first", "refs/heads/second"}, recorded(t))
	})

	t.Run("not recorded when the changes are thrown away", func(t *testing.T) {
		cache, cacheCtx := newCache(t)

		commitUpdate(t, cacheCtx, "refs/heads/thrown-away")
		cache.Cleanup()

		require.Equal(t, []string{"refs/heads/first", "refs/heads/second"}, recorded(t))
	})

	t.Run("not recorded when the cache is gone", func(t *testing.T) {
		// The payload is built while the cache is live, and the hook only runs once it is gone,
		// which is what a forged or late hook request looks like.
		other, otherCtx := newCache(t)
		env, err := gitcmd.NewHooksPayload(otherCtx, cfg, repo, gittest.DefaultObjectHash, nil, nil, gitcmd.ReferenceTransactionHook, nil).Env()
		require.NoError(t, err)
		other.Cleanup()

		changes := fmt.Sprintf("%s %s refs/heads/late\n", gittest.DefaultObjectHash.ZeroOID, gittest.DefaultObjectHash.EmptyTreeOID)
		require.NoError(t, hookManager.ReferenceTransactionHook(ctx, ReferenceTransactionCommitted, []string{env}, strings.NewReader(changes)))

		require.Equal(t, []string{"refs/heads/first", "refs/heads/second"}, recorded(t))
	})

	t.Run("recorded right away without a cache", func(t *testing.T) {
		commitUpdate(t, ctx, "refs/heads/direct")
		require.Equal(t, []string{"refs/heads/first", "refs/heads/second", "refs/heads/direct"}, recorded(t))
	})
}
