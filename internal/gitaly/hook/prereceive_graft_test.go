package hook

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/transaction"
	"github.com/feder-cr/open_scion/internal/gitlab"
	"github.com/feder-cr/open_scion/internal/graft/identity"
	"github.com/feder-cr/open_scion/internal/grpc/backchannel"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// TestPrereceive_graftAgentActor checks that in standalone mode the access check
// is evaluated for the agent that authenticated the request, not for the
// client-supplied gl_id, and that nothing changes outside standalone mode or for
// callers that did not authenticate as an agent.
func TestPrereceive_graftAgentActor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc         string
		standalone   bool
		identity     *identity.Identity
		expectedGLID string
	}{
		{
			desc:         "standalone with agent identity uses the agent",
			standalone:   true,
			identity:     &identity.Identity{Name: "agent-a", Source: identity.SourceAgent},
			expectedGLID: "agent-a",
		},
		{
			desc:         "standalone with shared token keeps gl_id",
			standalone:   true,
			identity:     &identity.Identity{Name: "shared", Source: identity.SourceSharedToken},
			expectedGLID: "agent-b",
		},
		{
			desc:         "standalone without identity keeps gl_id",
			standalone:   true,
			expectedGLID: "agent-b",
		},
		{
			desc:         "not standalone keeps gl_id even with agent identity",
			identity:     &identity.Identity{Name: "agent-a", Source: identity.SourceAgent},
			expectedGLID: "agent-b",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			cfg.Graft.Standalone = tc.standalone

			repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})

			payloadCtx := ctx
			if tc.identity != nil {
				payloadCtx = identity.NewContext(ctx, *tc.identity)
			}

			env, err := gitcmd.NewHooksPayload(
				payloadCtx,
				cfg,
				repo,
				gittest.DefaultObjectHash,
				nil,
				&gitcmd.UserDetails{UserID: "agent-b", Username: "agent-b", Protocol: "web"},
				gitcmd.PreReceiveHook,
				nil,
			).Env()
			require.NoError(t, err)

			var glID string
			gitlabAPI := prereceiveAPIMock{
				allowed: func(_ context.Context, params gitlab.AllowedParams) (bool, string, error) {
					glID = params.GLID
					return true, "", nil
				},
				prereceive: func(context.Context, string) (bool, error) {
					return true, nil
				},
			}

			hookManager := NewManager(
				cfg,
				config.NewLocator(cfg),
				testhelper.SharedLogger(t),
				gittest.NewCommandFactory(t, cfg),
				nil,
				transaction.NewManager(cfg, testhelper.SharedLogger(t), backchannel.NewRegistry()),
				&gitlabAPI,
				NewPostReceiveRegistry(),
			)

			var stdout, stderr bytes.Buffer
			require.NoError(t, hookManager.PreReceiveHook(ctx, repo, nil, []string{env}, strings.NewReader("changes\n"), &stdout, &stderr))
			require.Equal(t, tc.expectedGLID, glID)
		})
	}
}
