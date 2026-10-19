package hook

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// recordingVoter records the phases the hook manager hands to the storage core.
type recordingVoter struct {
	mu     sync.Mutex
	phases []string
}

func (v *recordingVoter) record(phase string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.phases = append(v.phases, phase)
}

func (v *recordingVoter) recorded() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.phases...)
}

func (v *recordingVoter) Preparing(context.Context, gitcmd.HooksPayload, []byte) {
	v.record("preparing")
}

func (v *recordingVoter) Denied(context.Context, gitcmd.HooksPayload, []byte) {
	v.record("denied")
}

func (v *recordingVoter) Prepared(context.Context, gitcmd.HooksPayload, []byte, changelog.Meta) error {
	v.record("prepared")
	return nil
}

func (v *recordingVoter) Committed(context.Context, gitcmd.HooksPayload, []byte) {
	v.record("committed")
}

func (v *recordingVoter) Aborted(context.Context, gitcmd.HooksPayload, []byte) {
	v.record("aborted")
}

func (v *recordingVoter) Authoritative() bool { return true }

func (v *recordingVoter) DeferredReferenceDurability() bool { return false }

// TestReferenceTransactionHook_storageVoterPhases checks that the hook manager
// hands every phase of a reference transaction to the storage core, the
// Preparing phase included (doc/graft/PLAN6.md, LOCK AND HISTORY RULE), and
// that a transaction the standalone policy denies is not prepared: nothing is
// uploaded for a transaction that the Prepared phase aborts.
func TestReferenceTransactionHook_storageVoterPhases(t *testing.T) {
	t.Parallel()

	changes := fmt.Sprintf("%s %s refs/heads/main\n", gittest.DefaultObjectHash.ZeroOID, strings.Repeat("b", gittest.DefaultObjectHash.EncodedLen()))

	for _, tc := range []struct {
		desc   string
		policy []config.GraftPolicyRule
		want   []string
		denied bool
	}{
		{
			desc: "every phase reaches the storage core",
			want: []string{"preparing", "prepared", "committed"},
		},
		{
			// The voter learns of its start (Denied), so that it counts the
			// transaction until its abort, but prepares nothing.
			desc:   "a denied transaction is neither prepared nor committed",
			policy: []config.GraftPolicyRule{{Refs: []string{"refs/heads/main"}, Allow: false}},
			want:   []string{"denied"},
			denied: true,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			cfg.Graft.Standalone = true
			cfg.Graft.Policy = tc.policy

			repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})

			payloadCtx := identity.NewContext(ctx, identity.Identity{Name: "agent-a", Source: identity.SourceAgent})
			env, err := gitcmd.NewHooksPayload(payloadCtx, cfg, repo, gittest.DefaultObjectHash, nil, nil, gitcmd.ReferenceTransactionHook, nil).Env()
			require.NoError(t, err)

			voter := &recordingVoter{}
			manager := NewManager(
				cfg,
				config.NewLocator(cfg),
				testhelper.SharedLogger(t),
				gittest.NewCommandFactory(t, cfg),
				nil,
				transaction.NewManager(cfg, testhelper.SharedLogger(t), backchannel.NewRegistry()),
				gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
				NewPostReceiveRegistry(),
				WithStorageVoter(voter),
			)

			require.NoError(t, manager.ReferenceTransactionHook(ctx, ReferenceTransactionPreparing, []string{env}, strings.NewReader(changes)))

			err = manager.ReferenceTransactionHook(ctx, ReferenceTransactionPrepared, []string{env}, strings.NewReader(changes))
			if tc.denied {
				require.Equal(t, codes.PermissionDenied, status.Code(err), "error %v", err)
				require.Equal(t, tc.want, voter.recorded())
				return
			}
			require.NoError(t, err)

			require.NoError(t, manager.ReferenceTransactionHook(ctx, ReferenceTransactionCommitted, []string{env}, strings.NewReader(changes)))
			require.Equal(t, tc.want, voter.recorded())
		})
	}
}
