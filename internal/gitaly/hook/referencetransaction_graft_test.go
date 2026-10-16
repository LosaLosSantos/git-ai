package hook

import (
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestReferenceTransactionHook_graftPolicy checks that in standalone mode the
// reference-transaction hook enforces the [[graft.policy]] rules in the
// prepared state for updates of a caller authenticated with an agent token, so
// that RPCs which do not run the pre-receive hook cannot bypass the policy. It
// also pins that nothing changes outside standalone mode, without a policy,
// for callers that are not agents and in the other states. HEAD is governed like
// the references under refs/.
func TestReferenceTransactionHook_graftPolicy(t *testing.T) {
	t.Parallel()

	policy := []config.GraftPolicyRule{
		{Actor: "planner", Refs: []string{"refs/heads/main"}, Allow: true},
		{Refs: []string{"refs/heads/main"}, Allow: false},
		{Refs: []string{"refs/heads/**"}, Allow: true},
	}

	agent := func(name string) *identity.Identity {
		return &identity.Identity{Name: name, Source: identity.SourceAgent}
	}

	oldOID := gittest.DefaultObjectHash.EmptyTreeOID
	zeroOID := gittest.DefaultObjectHash.ZeroOID
	update := func(ref string) string {
		return fmt.Sprintf("%s %s %s\n", zeroOID, oldOID, ref)
	}

	for _, tc := range []struct {
		desc       string
		standalone bool
		policy     []config.GraftPolicyRule
		identity   *identity.Identity
		state      ReferenceTransactionState
		changes    string
		denied     bool
	}{
		{
			desc:       "agent denied on a protected branch",
			standalone: true,
			policy:     policy,
			identity:   agent("builder"),
			state:      ReferenceTransactionPrepared,
			changes:    update("refs/heads/main"),
			denied:     true,
		},
		{
			desc:       "agent denied on a protected branch among allowed ones",
			standalone: true,
			policy:     policy,
			identity:   agent("builder"),
			state:      ReferenceTransactionPrepared,
			changes:    update("refs/heads/feature") + update("refs/heads/main"),
			denied:     true,
		},
		{
			desc:       "agent denied a force deletion of a protected branch",
			standalone: true,
			policy:     policy,
			identity:   agent("builder"),
			state:      ReferenceTransactionPrepared,
			changes:    fmt.Sprintf("%s %s refs/heads/main\n", zeroOID, zeroOID),
			denied:     true,
		},
		{
			desc:       "agent denied on a reference no rule governs",
			standalone: true,
			policy:     policy,
			identity:   agent("builder"),
			state:      ReferenceTransactionPrepared,
			changes:    update("refs/keep-around/abc"),
			denied:     true,
		},
		{
			desc:       "agent allowed on an open branch whose name holds Unicode whitespace",
			standalone: true,
			policy:     policy,
			identity:   agent("builder"),
			state:      ReferenceTransactionPrepared,
			changes:    update("refs/heads/a\u00a0b") + update("refs/heads/x\u0085y") + update("refs/heads/caf\xe9"),
		},
		{
			desc:       "agent denied on a reference whose name holds Unicode whitespace that no rule governs",
			standalone: true,
			policy:     policy,
			identity:   agent("builder"),
			state:      ReferenceTransactionPrepared,
			changes:    update("refs/tags/v\u2000main"),
			denied:     true,
		},
		{
			desc:       "privileged agent allowed on a protected branch",
			standalone: true,
			policy:     policy,
			identity:   agent("planner"),
			state:      ReferenceTransactionPrepared,
			changes:    update("refs/heads/main"),
		},
		{
			desc:       "agent allowed on an open branch",
			standalone: true,
			policy:     policy,
			identity:   agent("builder"),
			state:      ReferenceTransactionPrepared,
			changes:    update("refs/heads/feature/x"),
		},
		{
			desc:       "HEAD is governed: no rule covers it",
			denied:     true,
			standalone: true,
			policy:     policy,
			identity:   agent("builder"),
			state:      ReferenceTransactionPrepared,
			changes:    "ref:refs/heads/main ref:refs/heads/feature HEAD\n",
		},
		{
			desc:       "shared token caller is not restricted",
			standalone: true,
			policy:     policy,
			identity:   &identity.Identity{Name: "shared", Source: identity.SourceSharedToken},
			state:      ReferenceTransactionPrepared,
			changes:    update("refs/heads/main"),
		},
		{
			desc:       "caller without identity is not restricted",
			standalone: true,
			policy:     policy,
			state:      ReferenceTransactionPrepared,
			changes:    update("refs/heads/main"),
		},
		{
			desc:       "empty policy allows everything",
			standalone: true,
			identity:   agent("builder"),
			state:      ReferenceTransactionPrepared,
			changes:    update("refs/heads/main"),
		},
		{
			desc:     "not standalone keeps upstream behaviour",
			policy:   policy,
			identity: agent("builder"),
			state:    ReferenceTransactionPrepared,
			changes:  update("refs/heads/main"),
		},
		{
			desc:       "committed state does not evaluate the policy",
			standalone: true,
			policy:     policy,
			identity:   agent("builder"),
			state:      ReferenceTransactionCommitted,
			changes:    update("refs/heads/main"),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			cfg.Graft.Standalone = tc.standalone
			cfg.Graft.Policy = tc.policy
			cfg.Graft.Changelog.Disabled = true

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
				nil,
				gitcmd.ReferenceTransactionHook,
				nil,
			).Env()
			require.NoError(t, err)

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

			err = hookManager.ReferenceTransactionHook(ctx, tc.state, []string{env}, strings.NewReader(tc.changes))
			if !tc.denied {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			require.Equal(t, codes.PermissionDenied, status.Code(err), "got %v", err)
			require.Contains(t, err.Error(), "policy denies \"builder\"")
		})
	}
}
