package verify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/sdk"
)

// verifyToken is the shared token of the integration server.
const verifyToken = "graft-verify-test-token"

// realEnv is a standalone server with one empty repository and a verifier
// configuration that points at it.
type realEnv struct {
	t      *testing.T
	ctx    context.Context
	repo   *sdk.Repo
	cfg    Config
	runs   string
	gate   string
	client *sdk.Client
}

// startRealServer starts a standalone Gitaly, creates a repository and returns
// the environment. The verifier configuration uses the shared token.
func startRealServer(t *testing.T) *realEnv {
	t.Helper()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	cfg.Auth.Token = verifyToken
	cfg.Graft.Standalone = true

	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))),
	)
	cfg.SocketPath = addr

	ctx = testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))
	repoProto, _ := gittest.CreateRepository(t, ctx, cfg)

	storage := cfg.Storages[0].Name
	client, err := sdk.Dial(ctx, addr, sdk.WithStorage(storage), sdk.WithToken(verifyToken))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	dir := t.TempDir()
	script := filepath.Join(dir, "test.sh")
	require.NoError(t, os.WriteFile(script, []byte(testScript), 0o700))

	env := &realEnv{
		t:      t,
		ctx:    ctx,
		repo:   client.Repo(repoProto.GetRelativePath()),
		runs:   filepath.Join(dir, "runs.log"),
		gate:   filepath.Join(dir, "gate"),
		client: client,
	}
	require.NoError(t, os.WriteFile(env.gate, nil, 0o600))

	env.cfg = Config{
		Address:     addr,
		Token:       verifyToken,
		Storage:     storage,
		Repository:  repoProto.GetRelativePath(),
		Branch:      "main",
		WorkDir:     filepath.Join(dir, "work"),
		StateFile:   filepath.Join(dir, "state.json"),
		BatchWindow: Duration(20 * time.Millisecond),
		Test: TestConfig{
			Command: []string{"sh", script},
			Env:     []string{"RUNS=" + env.runs, "GATE=" + env.gate},
			Timeout: Duration(time.Minute),
		},
	}
	require.NoError(t, env.cfg.Finalize())
	return env
}

// land commits path on main, on top of the current tip, and returns the new
// tip. The first commit creates the branch.
func (env *realEnv) land(path, content string) string {
	env.t.Helper()

	startRev := "main"
	if _, err := env.repo.ResolveRevision(env.ctx, "refs/heads/main"); err != nil {
		startRev = ""
	}
	res, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
		Branch:   "main",
		StartRev: startRev,
		Message:  "add " + path,
		Author:   sdk.Signature{Name: "dev", Email: "dev@example.com"},
		User:     sdk.User{ID: "dev", Username: "dev", Name: "dev", Email: "dev@example.com"},
		Actions:  []sdk.Action{sdk.Put{Path: path, Content: []byte(content)}},
	})
	require.NoError(env.t, err)
	return res.CommitID
}

// green returns the green ref of the repository as the server sees it.
func (env *realEnv) green() string {
	env.t.Helper()

	oid, err := env.repo.ResolveRevision(env.ctx, "refs/graft/green/main")
	if sdk.IsNotFound(err) {
		return ""
	}
	require.NoError(env.t, err)
	return oid
}

// verifier opens a verifier over the real server.
func (env *realEnv) verifier() (*Verifier, func()) {
	env.t.Helper()

	v, closer, err := Open(env.ctx, env.cfg, discardLogger())
	require.NoError(env.t, err)
	return v, func() { _ = closer.Close() }
}

// runCount returns how many times the test command ran.
func (env *realEnv) runCount() int {
	data, err := os.ReadFile(env.runs)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(env.t, err)
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}

func TestRealServerRunOnceMovesGreenRef(t *testing.T) {
	env := startRealServer(t)
	v, closeVerifier := env.verifier()
	defer closeVerifier()

	tip := env.land("a.txt", "one")
	out, ran, err := v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.True(t, out.Passed, out.Output)
	require.True(t, out.GreenMoved)
	require.Equal(t, tip, out.Tip)
	require.Equal(t, tip, env.green())

	broken := env.land("broken", "x")
	out, ran, err = v.RunOnce(env.ctx)
	require.NoError(t, err)
	require.True(t, ran)
	require.False(t, out.Passed)
	require.Equal(t, broken, out.Tip)
	require.Equal(t, tip, env.green(), "a failing tip must not move the green ref")
	require.Equal(t, 2, env.runCount())
}

func TestRealServerRunFollowsWatchAndIgnoresGreenUpdates(t *testing.T) {
	env := startRealServer(t)
	v, closeVerifier := env.verifier()
	defer closeVerifier()

	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- v.Run(ctx) }()

	first := env.land("a.txt", "one")
	require.Eventually(t, func() bool { return env.green() == first }, 30*time.Second, 10*time.Millisecond)

	second := env.land("b.txt", "two")
	require.Eventually(t, func() bool { return env.green() == second }, 30*time.Second, 10*time.Millisecond)

	// The green moves are changesets of the log too. They must not start
	// further runs: exactly one run per landed tip.
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, 2, env.runCount())

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRealServerOpenRejectsWrongToken(t *testing.T) {
	env := startRealServer(t)
	env.cfg.Token = "wrong-token"

	_, closer, err := Open(env.ctx, env.cfg, discardLogger())
	if closer != nil {
		_ = closer.Close()
	}
	require.Error(t, err)
}

func TestRealServerRefUpdaterCompareAndSwap(t *testing.T) {
	env := startRealServer(t)
	tip := env.land("a.txt", "one")
	other := env.land("b.txt", "two")

	greens, err := dialRefUpdater(env.ctx, env.cfg.Address, env.cfg.Token, env.repo, env.cfg.Storage)
	require.NoError(t, err)
	defer func() { _ = greens.Close() }()

	current, err := greens.Current(env.ctx, env.cfg.GreenRef)
	require.NoError(t, err)
	require.Empty(t, current, "the green ref does not exist yet")

	require.NoError(t, greens.Move(env.ctx, env.cfg.GreenRef, "", tip))
	require.Equal(t, tip, env.green())

	// A move from a stale old value is refused and changes nothing.
	require.Error(t, greens.Move(env.ctx, env.cfg.GreenRef, zeroOIDLike(tip), other))
	require.Equal(t, tip, env.green())

	require.NoError(t, greens.Move(env.ctx, env.cfg.GreenRef, tip, other))
	require.Equal(t, other, env.green())
}
