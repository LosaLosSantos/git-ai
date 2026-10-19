package operations

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestNewServer_sharesTheResolverOfTheProcess: the operations servers of every listener take the
// resolver of the process from their dependencies, so that its check slots bound the process: a
// slot that one server holds is one the other cannot take.
func TestNewServer_sharesTheResolverOfTheProcess(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	cfg.Graft.Land.Resolver = config.GraftResolver{Enabled: true, Command: []string{"/bin/true"}, CheckConcurrency: 1}
	shared := resolver.NewShared(cfg.Graft.Land.Resolver)
	require.NoError(t, shared.Err)
	newDeps := func() *service.Dependencies {
		return &service.Dependencies{Cfg: cfg, Logger: testhelper.SharedLogger(t), GraftResolver: shared}
	}

	unixServer, tcpServer := NewServer(newDeps()), NewServer(newDeps())
	defer unixServer.landLanes.Close()
	defer tcpServer.landLanes.Close()
	require.Same(t, unixServer.conflictResolver, tcpServer.conflictResolver)

	ctx := testhelper.Context(t)
	slot, err := unixServer.conflictResolver.AcquireCheckSlot(ctx)
	require.NoError(t, err)
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = tcpServer.conflictResolver.AcquireCheckSlot(waitCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the bound is the process's, not the listener's")
	slot.Release()
}

// TestNewServer_resolverThatCannotBeBuilt: a resolver configuration that fails to build at run time
// (its token file gone since the configuration was validated) gives no resolver and the error,
// whether the server builds it or takes it from its dependencies.
func TestNewServer_resolverThatCannotBeBuilt(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	cfg.Graft.Land.Resolver = config.GraftResolver{Enabled: true, URL: "https://resolver.example/", TokenFile: filepath.Join(t.TempDir(), "gone")}

	alone := NewServer(&service.Dependencies{Cfg: cfg, Logger: testhelper.SharedLogger(t)})
	defer alone.landLanes.Close()
	require.Nil(t, alone.conflictResolver)
	require.ErrorContains(t, alone.resolverErr, "read token file")

	withShared := NewServer(&service.Dependencies{Cfg: cfg, Logger: testhelper.SharedLogger(t), GraftResolver: resolver.NewShared(cfg.Graft.Land.Resolver)})
	defer withShared.landLanes.Close()
	require.Nil(t, withShared.conflictResolver)
	require.ErrorContains(t, withShared.resolverErr, "read token file")
}

func TestUserLand_relandRobustness(t *testing.T) {
	t.Parallel()

	t.Run("a batch that panics gives its check slot back", func(t *testing.T) {
		t.Parallel()

		_, argv := perCallChecker(t, checkAnswer("graft-resolve", "a", true, ""), checkAnswer("graft-resolve", "b", true, ""))
		fx, s, req := batchedReland(t, argv)
		s.resolveCfg.CheckConcurrency = 1
		client, err := resolver.New(s.resolveCfg)
		require.NoError(t, err)
		s.conflictResolver = client
		s.landCheckBatchHook = func() { panic("a batch failed") }

		require.PanicsWithValue(t, "a batch failed", func() { _, _ = s.landPrecheckResolution(fx.ctx, req, fx.repo) })

		// The panic was recovered (as the gRPC server does): the only slot is free again.
		ctx, cancel := context.WithTimeout(fx.ctx, time.Second)
		defer cancel()
		slot, err := client.AcquireCheckSlot(ctx)
		require.NoError(t, err, "the slot of the panicking batch was given back")
		slot.Release()
	})

	t.Run("a pointer line that a side has of its own is not refused", func(t *testing.T) {
		t.Parallel()

		const pointer = "graft-conflict-pointer: an example of the line\n"
		fx := newResolveFixtureWith(t, config.GraftResolver{},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "base\n"}, {Mode: "100644", Path: "b", Content: "doc\n" + pointer + "base\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "ours\n"}, {Mode: "100644", Path: "b", Content: "doc\n" + pointer + "ours\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "theirs\n"}, {Mode: "100644", Path: "b", Content: "doc\n" + pointer + "theirs\n"}},
		)
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "agent-a\n", "doc\n"+pointer+"agent-b\n")
		_, err := fx.client.UserLand(fx.ctx, req)
		require.NoError(t, err)

		// a's sides have no such line: there it is still a pointer left in place.
		fx = newResolveFixture(t, config.GraftResolver{})
		req = fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "x\n"+pointer, "agent-b\n")
		_, err = fx.client.UserLand(fx.ctx, req)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, packageFile(t, pkg, "a").GetOutcome())
		require.Contains(t, packageFile(t, pkg, "a").GetReason(), "the pointer to a binary side")
	})

	t.Run("a branch that is a symbolic reference is checked at its target's tip", func(t *testing.T) {
		t.Parallel()

		_, argv := perCallChecker(t, checkAnswer("graft-resolve", "a", true, ""), checkAnswer("graft-resolve", "b", true, ""))
		fx, s, req := batchedReland(t, argv)
		gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "symbolic-ref", "refs/heads/alias", git.NewReferenceNameFromBranchName(git.DefaultBranch).String())
		req.Branch = []byte("alias")

		pre, err := s.landPrecheckResolution(fx.ctx, req, fx.repo)
		require.NoError(t, err)
		require.Empty(t, pre.skipped, "the tip of the alias is the tip of main")
		require.Equal(t, fx.tip, pre.tip)
		require.Empty(t, pre.result.failure)
		require.Len(t, pre.result.verdicts, 2)
	})
}
