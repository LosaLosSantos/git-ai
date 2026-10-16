package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// TestReferenceTransactionPreparingIsForwardedInAuthoritativeMode checks that
// gitaly-hooks forwards the "preparing" state of git's own reference
// transactions to the server only when the Graft storage core is
// authoritative (its write path uploads the pack before git locks the
// references), and keeps the upstream behaviour (no RPC) otherwise.
func TestReferenceTransactionPreparingIsForwardedInAuthoritativeMode(t *testing.T) {
	for _, tc := range []struct {
		desc        string
		storage     config.GraftStorage
		standalone  bool
		wantForward bool
	}{
		{desc: "storage core off", wantForward: false},
		{desc: "shadow mode", storage: config.GraftStorage{Mode: "shadow", Database: "sqlite:///x.db", Bucket: "mem://"}, wantForward: false},
		{desc: "authoritative mode", storage: config.GraftStorage{Mode: "authoritative", Database: "sqlite:///x.db", Bucket: "mem://"}, standalone: true, wantForward: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{
				Auth:  auth.Config{Token: "abc123"},
				Hooks: config.Hooks{CustomHooksDir: testhelper.TempDir(t)},
			}))
			cfg.Graft.Standalone = tc.standalone
			cfg.Graft.Storage = tc.storage

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})

			var mu sync.Mutex
			var states []gitalypb.ReferenceTransactionHookRequest_State
			runHookServiceWithMockServer(t, cfg, &hookMockServer{
				referenceTransactionHook: func(server gitalypb.HookService_ReferenceTransactionHookServer) error {
					req, err := server.Recv()
					if err != nil {
						return err
					}
					mu.Lock()
					states = append(states, req.GetState())
					mu.Unlock()
					return server.Send(&gitalypb.ReferenceTransactionHookResponse{
						ExitStatus: &gitalypb.ExitStatus{Value: 0},
					})
				},
			})

			gitalyHooksPath := testcfg.BuildGitalyHooks(t, cfg)
			testcfg.BuildGitalySSH(t, cfg)

			hookPath := filepath.Join(t.TempDir(), "reference-transaction")
			require.NoError(t, os.Symlink(gitalyHooksPath, hookPath))

			var stdout, stderr, logOutput bytes.Buffer
			cmd, err := command.New(ctx,
				capturedOutput{Logger: testhelper.SharedLogger(t), output: &logOutput},
				[]string{hookPath, "preparing"},
				command.WithSubprocessLogger(cfg.Logging.Config),
				command.WithEnvironment(envForHooks(t, ctx, cfg, repo,
					glHookValues{GLID: glID, GLUsername: glUsername, GLProtocol: "ssh", RemoteIP: remoteIP},
					proxyValues{})),
				command.WithStdin(strings.NewReader(gittest.DefaultObjectHash.ZeroOID.String()+" "+gittest.DefaultObjectHash.ZeroOID.String()+" refs/heads/x\n")),
				command.WithStdout(&stdout),
				command.WithStderr(&stderr),
				command.WithDir(repoPath),
			)
			require.NoError(t, err)
			require.NoError(t, cmd.Wait(), "stderr: %s", stderr.String())

			mu.Lock()
			defer mu.Unlock()
			if tc.wantForward {
				require.Equal(t, []gitalypb.ReferenceTransactionHookRequest_State{gitalypb.ReferenceTransactionHookRequest_PREPARING}, states)
			} else {
				require.Empty(t, states, "the preparing state is not forwarded")
			}
		})
	}
}
