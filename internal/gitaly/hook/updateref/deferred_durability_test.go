package updateref

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// durabilityVoter is a storage voter that only answers whether it defers the durability of the
// references.
type durabilityVoter struct {
	deferred bool
}

func (durabilityVoter) Preparing(context.Context, gitcmd.HooksPayload, []byte) {}

func (durabilityVoter) Denied(context.Context, gitcmd.HooksPayload, []byte) {}

func (durabilityVoter) Prepared(context.Context, gitcmd.HooksPayload, []byte, changelog.Meta) error {
	return nil
}

func (durabilityVoter) Committed(context.Context, gitcmd.HooksPayload, []byte) {}

func (durabilityVoter) Aborted(context.Context, gitcmd.HooksPayload, []byte) {}

func (durabilityVoter) Authoritative() bool { return true }

func (v durabilityVoter) DeferredReferenceDurability() bool { return v.deferred }

// TestDeferReferenceDurability verifies that the updater writes references without fsync only
// when the storage voter of its hook manager defers their durability: never without the storage
// core, nor when the voter does not (shadow mode, or authoritative mode where the kernel exposes
// no boot identity; the voter's own answer is tested in the voter package).
func TestDeferReferenceDurability(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	newManager := func(opts ...hook.ManagerOption) hook.Manager {
		return hook.NewManager(cfg, config.NewLocator(cfg), testhelper.NewLogger(t), gittest.NewCommandFactory(t, cfg), nil, nil,
			gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
			hook.NewPostReceiveRegistry(), opts...)
	}

	for _, tc := range []struct {
		desc     string
		manager  hook.Manager
		expected bool
	}{
		{desc: "storage core off", manager: newManager()},
		{desc: "voter keeps the fsync", manager: newManager(hook.WithStorageVoter(durabilityVoter{deferred: false}))},
		{desc: "voter defers the durability", manager: newManager(hook.WithStorageVoter(durabilityVoter{deferred: true})), expected: true},
		{desc: "manager without a storage voter", manager: hook.NewMockManager(t, hook.NopPreReceive, hook.NopPostReceive, hook.NopUpdate, hook.NopReferenceTransaction)},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.expected, deferReferenceDurability(tc.manager))
		})
	}
}
