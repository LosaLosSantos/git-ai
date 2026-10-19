package gitcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/identity"
)

// TestHiddenRefPrefixes_agents: git upload-pack and receive-pack hide refs/graft/ from an agent
// (agentrefs), on top of the internal prefixes every caller has hidden; another caller sees what
// the internal prefixes allow, graft-verify's green references included.
func TestHiddenRefPrefixes_agents(t *testing.T) {
	t.Parallel()

	values := func(opts []GlobalOption, key string) []string {
		var out []string
		for _, o := range opts {
			if p, ok := o.(ConfigPair); ok && p.Key == key {
				out = append(out, p.Value)
			}
		}
		return out
	}
	agent := identity.NewContext(context.Background(), identity.Identity{Name: "agent-a", Source: identity.SourceAgent})
	other := context.Background()

	require.Contains(t, values(hiddenUploadPackRefPrefixes(agent), "uploadpack.hideRefs"), "refs/graft/")
	require.Contains(t, values(hiddenReceivePackRefPrefixes(agent), "receive.hideRefs"), "refs/graft/")
	require.NotContains(t, values(hiddenUploadPackRefPrefixes(other), "uploadpack.hideRefs"), "refs/graft/")
	require.Contains(t, values(hiddenUploadPackRefPrefixes(other), "uploadpack.hideRefs"), "refs/graft/proposals/")
}
