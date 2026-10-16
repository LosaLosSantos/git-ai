package gitlab

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

const (
	policyTestOld = "0000000000000000000000000000000000000000"
	policyTestNew = "2222222222222222222222222222222222222222"
)

func policyTestChanges(refs ...string) string {
	var b strings.Builder
	for _, ref := range refs {
		fmt.Fprintf(&b, "%s %s %s\n", policyTestOld, policyTestNew, ref)
	}
	return b.String()
}

// TestEvaluatePolicy_HEAD checks that HEAD is decided by the policy like any
// other reference: it is allowed only by a rule that names it.
func TestEvaluatePolicy_HEAD(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc    string
		policy  []config.GraftPolicyRule
		changes string
		allowed bool
	}{
		{
			desc:    "HEAD is denied when only branch rules exist",
			policy:  []config.GraftPolicyRule{{Refs: []string{"refs/heads/**"}, Allow: true}},
			changes: policyTestChanges("HEAD"),
			allowed: false,
		},
		{
			desc:    "HEAD is allowed by a rule that names it",
			policy:  []config.GraftPolicyRule{{Refs: []string{"HEAD"}, Allow: true}},
			changes: policyTestChanges("HEAD"),
			allowed: true,
		},
		{
			desc:    "a wildcard rule covers HEAD",
			policy:  []config.GraftPolicyRule{{Allow: true}},
			changes: policyTestChanges("HEAD"),
			allowed: true,
		},
		{
			desc:    "HEAD and a branch are both needed",
			policy:  []config.GraftPolicyRule{{Refs: []string{"refs/heads/**"}, Allow: true}, {Refs: []string{"HEAD"}, Allow: true}},
			changes: policyTestChanges("refs/heads/main", "HEAD"),
			allowed: true,
		},
		{
			desc:    "one denied HEAD among allowed branches denies the change set",
			policy:  []config.GraftPolicyRule{{Refs: []string{"refs/heads/**"}, Allow: true}},
			changes: policyTestChanges("refs/heads/main", "HEAD"),
			allowed: false,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			allowed, _, err := EvaluatePolicy(tc.policy, "agent-a", tc.changes)
			require.NoError(t, err)
			require.Equal(t, tc.allowed, allowed)
		})
	}
}

// TestEvaluatePolicy_DenialMessages checks the denial messages. They describe
// the policy decision and do not carry the "GitLab:" prefix of GitLab denials.
func TestEvaluatePolicy_DenialMessages(t *testing.T) {
	t.Parallel()

	policy := []config.GraftPolicyRule{
		{Actor: "agent-*", Refs: []string{"refs/heads/feature/**"}, Allow: true},
		{Actor: "agent-*", Refs: []string{"refs/heads/main"}, Allow: false},
	}

	for _, tc := range []struct {
		desc        string
		actor       string
		changes     string
		wantMessage string
	}{
		{
			desc:        "explicit deny names the rule",
			actor:       "agent-a",
			changes:     policyTestChanges("refs/heads/main"),
			wantMessage: `policy denies "agent-a" to update "refs/heads/main" (rule 2)`,
		},
		{
			desc:        "no matching rule",
			actor:       "agent-a",
			changes:     policyTestChanges("HEAD"),
			wantMessage: `policy denies "agent-a" to update "HEAD": no matching rule`,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			allowed, message, err := EvaluatePolicy(policy, tc.actor, tc.changes)
			require.NoError(t, err)
			require.False(t, allowed)
			require.Equal(t, tc.wantMessage, message)
			require.False(t, strings.HasPrefix(message, "GitLab:"))
		})
	}
}

// TestStandaloneClient_DenialIsNotGitLabMessage checks the standalone client's
// denial for an update performed with the policy.
func TestStandaloneClient_DenialIsNotGitLabMessage(t *testing.T) {
	t.Parallel()

	client := NewStandaloneClient(config.Graft{
		Standalone: true,
		Policy:     []config.GraftPolicyRule{{Refs: []string{"refs/heads/**"}, Allow: true}},
	}, testhelper.SharedLogger(t))

	allowed, message, err := client.Allowed(context.Background(), AllowedParams{
		GLID:    "agent-a",
		Changes: policyTestChanges("HEAD"),
	})
	require.NoError(t, err)
	require.False(t, allowed)
	require.NotEmpty(t, message)
	require.False(t, strings.HasPrefix(message, "GitLab:"))
}

// TestEvaluatePolicy_InvalidInput checks that a malformed change set and an
// invalid glob are reported as errors, not as decisions.
func TestEvaluatePolicy_InvalidInput(t *testing.T) {
	t.Parallel()

	t.Run("malformed change", func(t *testing.T) {
		t.Parallel()

		allowed, _, err := EvaluatePolicy([]config.GraftPolicyRule{{Allow: true}}, "agent-a", "garbage\n")
		require.ErrorContains(t, err, "malformed change")
		require.False(t, allowed)
	})

	t.Run("invalid ref glob", func(t *testing.T) {
		t.Parallel()

		allowed, _, err := EvaluatePolicy([]config.GraftPolicyRule{{Refs: []string{"refs/[heads"}, Allow: true}}, "agent-a", policyTestChanges("HEAD"))
		require.Error(t, err)
		require.False(t, allowed)
	})
}
