package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service/setup"
	"github.com/feder-cr/open_scion/internal/gitlab"
	"github.com/feder-cr/open_scion/internal/graft/workspace"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/sdk"
)

// startPolicyServer starts a standalone server with the agent alice and a policy
// that lets only alice update main and her own temporary branches.
func startPolicyServer(t *testing.T) serverEnv {
	t.Helper()
	return startPolicyServerFor(t, "alice", "alice-secret")
}

// startPolicyServerFor is startPolicyServer for an agent with the given name and
// token, whose policy is written for graft/<name>/** as the documentation says.
func startPolicyServerFor(t *testing.T, agent, token string) serverEnv {
	t.Helper()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	cfg.Graft.Agents = []config.GraftAgent{{Name: agent, Token: token}}
	cfg.Graft.Policy = []config.GraftPolicyRule{
		{Actor: agent, Refs: []string{"refs/heads/main", "refs/heads/graft/" + agent + "/**"}, Allow: true},
		{Actor: "*", Allow: false},
	}
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))))
	cfg.SocketPath = addr

	// The repository is created on disk because every RPC of the server needs the
	// agent token, and the test helper's creation path does not present one.
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	env := serverEnv{
		ctx:      ctx,
		cfg:      cfg,
		addr:     addr,
		storage:  cfg.Storages[0].Name,
		relPath:  repoProto.GetRelativePath(),
		repoPath: repoPath,
	}
	env.seedMain(t, map[string]string{"a.txt": "one\n"})
	return env
}

// TestSDKBackendWritesAsConfiguredUser checks that a commit passes the server's
// access policy as the configured user, on a temporary branch named after that
// user, and that the changeset log records the identity of the agent token.
func TestSDKBackendWritesAsConfiguredUser(t *testing.T) {
	t.Parallel()

	env := startPolicyServer(t)
	ctx := env.ctx
	b, err := openSDKBackend(ctx, target{Address: env.addr, Token: "alice-secret", Storage: env.storage, Repo: env.relPath, User: "alice"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	dir := filepath.Join(t.TempDir(), "ws")
	ws, err := workspace.Clone(ctx, dir, b, workspace.CloneOptions{
		Address: env.addr,
		Storage: env.storage,
		Repo:    env.relPath,
		Branch:  "main",
	})
	require.NoError(t, err)

	writeLocal(t, dir, "a.txt", "two\n")
	res, err := ws.Commit(ctx, workspace.CommitRequest{
		Message:    "change a",
		Author:     testAuthor(),
		User:       "alice",
		LandBranch: "main",
	})
	require.NoError(t, err)
	require.True(t, res.Landed)
	require.Contains(t, res.Branch, "graft/alice/")
	require.Equal(t, "two\n", readRemoteFile(t, env, b, "main", "a.txt"))

	changesets, err := b.Changesets(ctx, 0, 100)
	require.NoError(t, err)
	actors := map[string]bool{}
	for _, c := range changesets {
		actors[c.Actor] = true
	}
	require.True(t, actors["alice"], "the changeset log names the token identity: %v", actors)
	require.False(t, actors["anonymous"], "no write is recorded as anonymous: %v", actors)
}

// TestSDKBackendWithoutUserIsCheckedAsAnonymous checks that a commit without a
// configured user is made as the anonymous user (on a temporary branch named
// after the author) instead of failing in the hooks, that the policy, which
// decides on the agent of the token, refuses that branch, and that the refusal
// explains how to name the user.
func TestSDKBackendWithoutUserIsCheckedAsAnonymous(t *testing.T) {
	t.Parallel()

	env := startPolicyServer(t)
	ctx := env.ctx
	b, err := openSDKBackend(ctx, target{Address: env.addr, Token: "alice-secret", Storage: env.storage, Repo: env.relPath})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	dir := filepath.Join(t.TempDir(), "ws")
	ws, err := workspace.Clone(ctx, dir, b, workspace.CloneOptions{Address: env.addr, Storage: env.storage, Repo: env.relPath, Branch: "main"})
	require.NoError(t, err)

	writeLocal(t, dir, "a.txt", "two\n")
	_, err = ws.Commit(ctx, workspace.CommitRequest{Message: "change a", Author: testAuthor(), LandBranch: "main"})
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "%v", err)
	require.ErrorContains(t, err, `policy denies "alice" to update "refs/heads/graft/`)
	require.ErrorContains(t, err, "GRAFT_USER")
	require.Nil(t, ws.Summary().Pending, "a refused commit leaves nothing pending")
	require.Equal(t, "one\n", readRemoteFile(t, env, b, "main", "a.txt"))

	// Naming another user than the policy allows is refused too, without the hint.
	other, err := openSDKBackend(ctx, target{Address: env.addr, Token: "alice-secret", Storage: env.storage, Repo: env.relPath, User: "mallory"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	_, err = other.CommitFiles(ctx, workspace.CommitOptions{
		Branch:   "graft/mallory/x",
		StartRev: "main",
		Message:  "m",
		Author:   testAuthor(),
		Actions:  []workspace.Action{{Kind: workspace.ActionPut, Path: "m.txt", Content: []byte("m")}},
	})
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "%v", err)
	require.NotContains(t, err.Error(), "GRAFT_USER")
}

// TestCLIWritesAsUserFromFlagAndEnvironment drives commit and land through the
// command line with the user from -user and from GRAFT_USER. It is not parallel
// because it sets the environment.
func TestCLIWritesAsUserFromFlagAndEnvironment(t *testing.T) {
	env := startPolicyServer(t)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-token", "alice-secret", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	writeLocal(t, dir, "a.txt", "two\n")
	code, _, stderr = runCLI(t, "commit", "-C", dir, "-token", "alice-secret", "-m", "no user")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "GRAFT_USER")

	code, stdout, stderr := runCLI(t, "commit", "-C", dir, "-token", "alice-secret", "-user", "alice", "-m", "flag user")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "graft/alice/")
	code, stdout, stderr = runCLI(t, "land", "-C", dir, "-token", "alice-secret")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "landed at")

	t.Setenv("GRAFT_TOKEN", "alice-secret")
	t.Setenv("GRAFT_USER", "alice")
	writeLocal(t, dir, "a.txt", "three\n")
	code, stdout, stderr = runCLI(t, "commit", "-C", dir, "-m", "env user", "-land", "main")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "landed it on main")

	code, stdout, _ = runCLI(t, "cat", "-C", dir, "main:a.txt")
	require.Equal(t, exitOK, code)
	require.Equal(t, "three\n", stdout)
}
