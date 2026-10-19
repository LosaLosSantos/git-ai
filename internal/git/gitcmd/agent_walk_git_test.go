package gitcmd_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// TestAgentWalk_leavesHiddenRefsOut: whatever asks git to walk every reference, as an option, an
// argument or a line on stdin, the walk of an agent's command leaves out the references under
// refs/graft/, and another caller's walk still reaches them. The commit only a proposal reference
// reaches stands for a proposal.
func TestAgentWalk_leavesHiddenRefsOut(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	main := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "a\n"}))
	secret := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(main),
		gittest.WithReference("refs/graft/proposals/0123456789abcdef/1"), gittest.WithMessage("SECRET proposal"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "proposed\n"}))
	factory := gittest.NewCommandFactory(t, cfg)
	agent := identity.NewContext(ctx, identity.Identity{Name: "agent-a", Source: identity.SourceAgent})

	run := func(t *testing.T, ctx context.Context, sc gitcmd.Command, stdin string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		opts := []gitcmd.CmdOpt{gitcmd.WithStdout(&stdout), gitcmd.WithStderr(&stderr)}
		if stdin != "" {
			opts = append(opts, gitcmd.WithStdin(strings.NewReader(stdin)))
		}
		cmd, err := factory.New(ctx, repo, sc, opts...)
		require.NoError(t, err)
		require.NoError(t, cmd.Wait(), "stderr: %s", stderr.String())
		return stdout.String()
	}

	for _, tc := range []struct {
		desc  string
		sc    gitcmd.Command
		stdin string
		seen  string
	}{
		{desc: "rev-list --all", sc: gitcmd.Command{Name: "rev-list", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--all"}}}, seen: secret.String()},
		{desc: "rev-list --all as an argument", sc: gitcmd.Command{Name: "rev-list", Args: []string{"--all"}}, seen: secret.String()},
		{desc: "rev-list --glob=refs/*", sc: gitcmd.Command{Name: "rev-list", Args: []string{"--glob=refs/*"}}, seen: secret.String()},
		{desc: "rev-list --not --all", sc: gitcmd.Command{Name: "rev-list", Args: []string{secret.String(), "--not", "--all"}}, seen: ""},
		{desc: "rev-list --all on stdin", sc: gitcmd.Command{Name: "rev-list", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--stdin"}}}, stdin: "--all\n", seen: secret.String()},
		{desc: "rev-list --glob= on stdin", sc: gitcmd.Command{Name: "rev-list", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--stdin"}}}, stdin: "--glob=refs/graft/*\n", seen: secret.String()},
		{desc: "log --all", sc: gitcmd.Command{Name: "log", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--format=%H"}, gitcmd.Flag{Name: "--all"}}}, seen: secret.String()},
		{desc: "show --all", sc: gitcmd.Command{Name: "show", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--no-patch"}, gitcmd.Flag{Name: "--format=%H"}, gitcmd.Flag{Name: "--all"}}}, seen: secret.String()},
		{desc: "rev-parse --all", sc: gitcmd.Command{Name: "rev-parse", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--all"}}}, seen: secret.String()},
		{desc: "fast-export --all", sc: gitcmd.Command{Name: "fast-export", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--all"}}}, seen: "SECRET proposal"},
		{desc: "format-patch --all", sc: gitcmd.Command{Name: "format-patch", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--stdout"}, gitcmd.Flag{Name: "--all"}}}, seen: "SECRET proposal"},
		{desc: "bundle create --all", sc: gitcmd.Command{Name: "bundle", Action: "create", Flags: []gitcmd.Option{gitcmd.OutputToStdout, gitcmd.Flag{Name: "--all"}}}, seen: "refs/graft/"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			agentOut := run(t, agent, tc.sc, tc.stdin)
			otherOut := run(t, ctx, tc.sc, tc.stdin)
			if tc.seen == "" {
				// The proposal is no reference to an agent: it is new to it.
				require.Contains(t, agentOut, secret.String())
				require.Empty(t, otherOut)
				return
			}
			require.NotContains(t, agentOut, tc.seen)
			require.Contains(t, otherOut, tc.seen)
		})
	}

	t.Run("--branches is left alone", func(t *testing.T) {
		out := run(t, agent, gitcmd.Command{Name: "rev-list", Args: []string{"--branches"}}, "")
		require.Contains(t, out, main.String())
	})

	t.Run("--alternate-refs walks the pool's branches and tags only", func(t *testing.T) {
		member, memberPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
		_, poolPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
		base := gittest.WriteCommit(t, cfg, poolPath, gittest.WithReference(git.ObjectPoolRefNamespace+"/heads/main"))
		mirrored := gittest.WriteCommit(t, cfg, poolPath, gittest.WithParents(base), gittest.WithMessage("mirrored proposal"),
			gittest.WithReference(git.ObjectPoolRefNamespace+"/graft/proposals/0123456789abcdef/1"))
		require.NoError(t, os.WriteFile(filepath.Join(memberPath, "objects", "info", "alternates"), []byte(filepath.Join(poolPath, "objects")+"\n"), 0o644))

		walk := func(ctx context.Context) string {
			var stdout, stderr bytes.Buffer
			cmd, err := factory.New(ctx, member, gitcmd.Command{Name: "rev-list", Args: []string{"--alternate-refs"}},
				gitcmd.WithStdout(&stdout), gitcmd.WithStderr(&stderr))
			require.NoError(t, err)
			require.NoError(t, cmd.Wait(), "stderr: %s", stderr.String())
			return stdout.String()
		}
		agentOut := walk(agent)
		require.Contains(t, agentOut, base.String())
		require.NotContains(t, agentOut, mirrored.String())
		require.Contains(t, walk(ctx), mirrored.String())
	})

	t.Run("revisions written to stdin", func(t *testing.T) {
		var stdout bytes.Buffer
		cmd, err := factory.New(agent, repo, gitcmd.Command{Name: "rev-list", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--stdin"}}},
			gitcmd.WithSetupStdin(), gitcmd.WithStdout(&stdout))
		require.NoError(t, err)
		_, err = cmd.Write([]byte("--glob=refs/*\n"))
		require.NoError(t, err)
		require.NoError(t, cmd.Wait())
		require.Contains(t, stdout.String(), main.String())
		require.NotContains(t, stdout.String(), secret.String())
	})

	t.Run("a writer to a git that stopped reading does not block", func(t *testing.T) {
		var stderr bytes.Buffer
		cmd, err := factory.New(agent, repo, gitcmd.Command{Name: "rev-list", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--stdin"}}},
			gitcmd.WithSetupStdin(), gitcmd.WithStderr(&stderr))
		require.NoError(t, err)

		done := make(chan error, 1)
		go func() {
			line := []byte(strings.Repeat("0", 39) + "1\n")
			var err error
			for i := 0; i < 1<<16 && err == nil; i++ {
				if i == 0 {
					_, err = cmd.Write([]byte("no-such-revision\n"))
					continue
				}
				_, err = cmd.Write(line)
			}
			done <- err
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			require.FailNow(t, "the writes blocked on a git that exited")
		}
		require.Error(t, cmd.Wait())
		require.Contains(t, stderr.String(), "no-such-revision")
	})
}
