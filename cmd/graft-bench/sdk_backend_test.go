package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service/setup"
	"github.com/feder-cr/open_scion/internal/gitlab"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/sdk"
)

// benchToken is the shared token of the test server.
const benchToken = "bench-shared-token"

// startBenchServer starts a standalone server without agents and returns its
// address, the context to use with it and the relative path of an empty
// repository on its first storage.
func startBenchServer(t *testing.T) (addr string, ctx context.Context, storage, relPath string) {
	t.Helper()

	return startBenchServerWith(t, config.GraftResolver{})
}

// startBenchServerWith is startBenchServer with a conflict resolver configured
// for the land path.
func startBenchServerWith(t *testing.T, resolver config.GraftResolver) (addr string, ctx context.Context, storage, relPath string) {
	t.Helper()

	ctx = featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	cfg.Auth.Token = benchToken
	cfg.Graft.Standalone = true
	cfg.Graft.Land.Resolver = resolver

	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	addr = testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))),
	)
	cfg.SocketPath = addr

	ctx = testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))
	repoProto, _ := gittest.CreateRepository(t, ctx, cfg)

	return addr, ctx, cfg.Storages[0].Name, repoProto.GetRelativePath()
}

// TestSDKBackendAgainstServer runs the benchmark through the SDK backend on a
// real server. Changes that do not touch the shared file always land. Of the
// shared changes, the first lands and the others conflict, because each is
// based on the same empty shared file. In land-resolve mode the server has no
// resolver configured, so nothing is resolved and the outcome is the same.
func TestSDKBackendAgainstServer(t *testing.T) {
	t.Parallel()

	addr, ctx, storage, relPath := startBenchServer(t)
	client, err := sdk.Dial(ctx, addr, sdk.WithStorage(storage), sdk.WithToken(benchToken))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	be := NewSDKBackend(client, relPath)
	base := Config{Agents: 4, ChangesPerAgent: 3, Mode: ModeLand, Seed: 1, Branch: "bench-rate-zero"}

	t.Run("no shared changes", func(t *testing.T) {
		t.Parallel()

		cfg := base
		cfg.Branch = "bench-no-shared"
		res, err := Run(ctx, cfg, be)
		require.NoError(t, err)
		require.Equal(t, 12, res.Changes)
		require.Equal(t, 12, res.Landed)
		require.Zero(t, res.Failed, "errors: %v", res.Errors)
		require.Positive(t, res.P50MS)
	})

	t.Run("every change shared", func(t *testing.T) {
		t.Parallel()

		cfg := base
		cfg.Branch = "bench-all-shared"
		cfg.ConflictRate = 1
		res, err := Run(ctx, cfg, be)
		require.NoError(t, err)
		require.Equal(t, 12, res.Changes)
		require.Equal(t, 1, res.Landed)
		require.Equal(t, 11, res.Failed)
		require.Equal(t, 11, res.Conflicts, "errors: %v", res.Errors)
		require.Zero(t, res.Resolutions)
	})

	t.Run("land-resolve without a resolver", func(t *testing.T) {
		t.Parallel()

		cfg := base
		cfg.Branch = "bench-resolve-disabled"
		cfg.Mode = ModeLandResolve
		cfg.ConflictRate = 1
		res, err := Run(ctx, cfg, be)
		require.NoError(t, err)
		require.Equal(t, 1, res.Landed)
		require.Equal(t, 11, res.Conflicts)
		require.Zero(t, res.Resolutions)
	})

	t.Run("queue mode", func(t *testing.T) {
		t.Parallel()

		cfg := base
		cfg.Branch = "bench-queue"
		cfg.Mode = ModeQueue
		cfg.TestDuration = time.Millisecond
		res, err := Run(ctx, cfg, be)
		require.NoError(t, err)
		require.Equal(t, 12, res.Landed)
		require.Zero(t, res.Failed)
	})
}

// TestSDKBackendPrepareKeepsExistingBranch checks that Prepare returns the tip
// of a branch that already exists instead of creating it again.
func TestSDKBackendPrepareKeepsExistingBranch(t *testing.T) {
	t.Parallel()

	addr, ctx, storage, relPath := startBenchServer(t)
	client, err := sdk.Dial(ctx, addr, sdk.WithStorage(storage), sdk.WithToken(benchToken))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	be := NewSDKBackend(client, relPath)
	cfg := Config{Agents: 1, ChangesPerAgent: 1, Mode: ModeLand, Branch: "bench-prepare"}

	first, err := be.Prepare(ctx, cfg)
	require.NoError(t, err)
	second, err := be.Prepare(ctx, cfg)
	require.NoError(t, err)
	require.Equal(t, first, second)
}

// TestSDKBackendRefusesBranchOfEarlierRun checks that a second run on the
// branch of an earlier run stops in Prepare with a clear error, instead of
// failing every change at commit time because its file exists. A run on a
// fresh branch of the same repository still works.
func TestSDKBackendRefusesBranchOfEarlierRun(t *testing.T) {
	t.Parallel()

	addr, ctx, storage, relPath := startBenchServer(t)
	client, err := sdk.Dial(ctx, addr, sdk.WithStorage(storage), sdk.WithToken(benchToken))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	be := NewSDKBackend(client, relPath)
	cfg := Config{Agents: 2, ChangesPerAgent: 2, Mode: ModeLand, Seed: 1, Branch: DefaultBranch}
	res, err := Run(ctx, cfg, be)
	require.NoError(t, err)
	require.Equal(t, 4, res.Landed, "errors: %v", res.Errors)

	cfg.Mode = ModeQueue
	_, err = Run(ctx, cfg, be)
	require.ErrorIs(t, err, errBranchUsed)
	require.Contains(t, err.Error(), "fresh --branch")

	cfg.Branch = "bench-second"
	res, err = Run(ctx, cfg, be)
	require.NoError(t, err)
	require.Equal(t, 4, res.Landed, "errors: %v", res.Errors)
}

// buildGraftResolve builds the graft-resolve command of tools/graft-agents
// into a temporary directory.
func buildGraftResolve(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "graft-resolve")
	// GOPROXY=off keeps the build from the network. See buildAgentsTool of the
	// e2e tests for the reason.
	cmd := exec.Command("go", "-C", "../../tools/graft-agents", "build", "-o", bin, "./cmd/graft-resolve")
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build graft-resolve (run go -C tools/graft-agents mod download first if the module cache is cold): %s", out)
	return bin
}

// TestSDKBackendLandResolveWithResolver runs land-resolve mode against a
// server whose resolver is the real graft-resolve binary, with the trivial
// and union strategies for the shared file. Every shared change conflicts
// with the tip, so the server resolves them and every change lands. This is
// the path where the resolutions of the report are counted.
func TestSDKBackendLandResolveWithResolver(t *testing.T) {
	t.Parallel()

	resolveConfig := filepath.Join(t.TempDir(), "resolve.toml")
	require.NoError(t, os.WriteFile(resolveConfig, []byte(`[[strategy]]
type = "trivial"

[[strategy]]
type = "union"
paths = ["shared/log.txt"]
`), 0o600))

	addr, ctx, storage, relPath := startBenchServerWith(t, config.GraftResolver{
		Enabled: true,
		Command: []string{buildGraftResolve(t), "--config", resolveConfig},
	})
	client, err := sdk.Dial(ctx, addr, sdk.WithStorage(storage), sdk.WithToken(benchToken))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	be := NewSDKBackend(client, relPath)
	cfg := Config{Agents: 4, ChangesPerAgent: 3, Mode: ModeLandResolve, ConflictRate: 1, Seed: 1, Branch: "bench-resolve-union"}
	res, err := Run(ctx, cfg, be)
	require.NoError(t, err)
	require.Equal(t, 12, res.Changes)
	require.Equal(t, 12, res.Landed, "errors: %v", res.Errors)
	require.Zero(t, res.Failed, "errors: %v", res.Errors)
	require.Zero(t, res.Conflicts)
	require.GreaterOrEqual(t, res.Resolutions, 11, "the first change lands clean, the other eleven are resolved")
}

// TestCreateRepository checks that --create-repo creates the repository
// through the server, that the benchmark can use it, and that a second
// creation fails because the repository exists.
func TestCreateRepository(t *testing.T) {
	t.Parallel()

	addr, ctx, storage, _ := startBenchServer(t)
	const relPath = "bench-created.git"

	require.NoError(t, createRepository(ctx, addr, benchToken, storage, relPath))
	require.Equal(t, codes.AlreadyExists, sdk.Code(createRepository(ctx, addr, benchToken, storage, relPath)))

	client, err := sdk.Dial(ctx, addr, sdk.WithStorage(storage), sdk.WithToken(benchToken))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	base, err := NewSDKBackend(client, relPath).Prepare(ctx, Config{Branch: "main", Mode: ModeLand})
	require.NoError(t, err)
	require.NotEmpty(t, base)
}
