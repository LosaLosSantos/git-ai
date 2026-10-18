package hook

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// TestCustomHooksGetTheFsyncConfiguration checks that the git commands of a custom hook get the
// fsync configuration of the server's own git commands, as the command factory applies it: its
// defaults, overridden by the administrator's [git] configuration.
func TestCustomHooksGetTheFsyncConfiguration(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	for _, tc := range []struct {
		desc  string
		admin []config.GitConfig
	}{
		{desc: "defaults"},
		{desc: "administrator override", admin: []config.GitConfig{{Key: "core.fsync", Value: "all"}, {Key: "core.fsyncMethod", Value: "writeout-only"}}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			cfg := testcfg.Build(t)
			cfg.Git.Config = tc.admin
			factory := gittest.NewCommandFactory(t, cfg)

			// What the command factory gives every git command, the administrator last.
			global, err := factory.GlobalConfiguration(ctx)
			require.NoError(t, err)
			want := map[string]string{}
			for _, pair := range append(global, cfg.Git.Config...) {
				if pair.Key == "core.fsync" || pair.Key == "core.fsyncMethod" {
					want[pair.Key] = pair.Value
				}
			}
			require.Len(t, want, 2)

			manager := NewManager(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t), factory, nil, &transaction.MockManager{},
				gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive), NewPostReceiveRegistry())

			repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
			env, err := manager.customHooksEnv(ctx, gitcmd.HooksPayload{Repo: repo, UserDetails: &gitcmd.UserDetails{}}, nil, nil)
			require.NoError(t, err)

			got := map[string]string{}
			count := environmentValue(env, "GIT_CONFIG_COUNT")
			require.Equal(t, "2", count)
			for _, i := range []string{"0", "1"} {
				got[environmentValue(env, "GIT_CONFIG_KEY_"+i)] = environmentValue(env, "GIT_CONFIG_VALUE_"+i)
			}
			require.Equal(t, want, got)
		})
	}
}
