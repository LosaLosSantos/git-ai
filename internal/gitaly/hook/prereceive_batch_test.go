package hook

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/transaction"
	"github.com/feder-cr/open_scion/internal/gitlab"
	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/grpc/backchannel"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// TestPreReceiveHook_batchAdmitted checks that the access check of the
// pre-receive hook is skipped exactly for the combined update of a batched land
// in standalone mode. Its members were admitted one by one by the batch
// executor; the combined update runs as the synthetic graft-batch user, which a
// policy protecting the branch would otherwise always deny. Every other update
// is still checked.
func TestPreReceiveHook_batchAdmitted(t *testing.T) {
	t.Parallel()

	members := []changelog.Member{{OriginalCommit: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40), Actor: "planner"}}

	for _, tc := range []struct {
		desc         string
		standalone   bool
		operation    string
		members      []changelog.Member
		expectCheck  bool
		expectDenied bool
	}{
		{desc: "standalone batched land with members skips the check", standalone: true, operation: changelog.OperationLandBatch, members: members},
		{desc: "standalone batched land without members is checked", standalone: true, operation: changelog.OperationLandBatch, expectCheck: true, expectDenied: true},
		{desc: "standalone land with members is checked", standalone: true, operation: "land", members: members, expectCheck: true, expectDenied: true},
		{desc: "standalone plain update is checked", standalone: true, operation: "ff_branch", expectCheck: true, expectDenied: true},
		{desc: "GitLab mode batched land with members is checked", standalone: false, operation: changelog.OperationLandBatch, members: members, expectCheck: true, expectDenied: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			cfg.Graft.Standalone = tc.standalone
			cfg.Graft.Changelog.Disabled = true

			repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})
			repo.GlRepository = "project-1"

			var checks atomic.Int32
			gitlabClient := gitlab.NewMockClient(t,
				func(context.Context, gitlab.AllowedParams) (bool, string, error) {
					checks.Add(1)
					return false, "denied by test policy", nil
				},
				gitlab.MockPreReceive,
				gitlab.MockPostReceive,
			)

			hookManager := NewManager(
				cfg,
				config.NewLocator(cfg),
				testhelper.SharedLogger(t),
				gittest.NewCommandFactory(t, cfg),
				nil,
				transaction.NewManager(cfg, testhelper.SharedLogger(t), backchannel.NewRegistry()),
				gitlabClient,
				NewPostReceiveRegistry(),
			)

			payloadCtx := changelog.WithBatch(changelog.WithOperation(ctx, tc.operation), tc.members)
			env, err := gitcmd.NewHooksPayload(
				payloadCtx,
				cfg,
				repo,
				gittest.DefaultObjectHash,
				nil,
				&gitcmd.UserDetails{UserID: "graft-batch", Username: "graft-batch", Protocol: "web"},
				gitcmd.PreReceiveHook,
				featureflag.FromContext(ctx),
			).Env()
			require.NoError(t, err)

			changes := fmt.Sprintf("%s %s refs/heads/main\n", gittest.DefaultObjectHash.ZeroOID, strings.Repeat("b", 40))

			var stdout, stderr bytes.Buffer
			err = hookManager.PreReceiveHook(ctx, repo, nil, []string{env}, strings.NewReader(changes), &stdout, &stderr)

			if tc.expectDenied {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			if tc.expectCheck {
				require.Equal(t, int32(1), checks.Load(), "access check must run")
			} else {
				require.Zero(t, checks.Load(), "access check must be skipped")
			}
		})
	}
}
