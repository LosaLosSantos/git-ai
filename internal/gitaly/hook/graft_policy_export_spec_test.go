package hook

// Black-box spec test for the exported wrapper EvaluateGraftPolicy (WP22),
// written from doc/graft/PLAN3.md only.

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/stretchr/testify/require"
)

const (
	specWP22ZeroOID = "0000000000000000000000000000000000000000"
	specWP22NewOID  = "1111111111111111111111111111111111111111"
)

// The spec says EvaluateGraftPolicy is an exported wrapper over graftPolicyDenial
// with the same semantics. With an empty graft configuration (no policy
// configured) an update must be allowed, with an empty message and no error.
func TestSpecEvaluateGraftPolicyEmptyConfigAllows(t *testing.T) {
	allowed, message, err := EvaluateGraftPolicy(config.Graft{}, "alice", "refs/heads/main", specWP22ZeroOID, specWP22NewOID)
	require.NoError(t, err, "evaluating a well-formed update must not return an error")
	require.True(t, allowed, "with no graft policy configured the update must be allowed")
	require.Empty(t, message, "an allowed update must not carry a denial message")
}

// The wrapper is a pure function of its inputs: repeated evaluation of the same
// update must give the same answer.
func TestSpecEvaluateGraftPolicyIsDeterministic(t *testing.T) {
	cfg := config.Graft{}
	a1, m1, err1 := EvaluateGraftPolicy(cfg, "alice", "refs/heads/main", specWP22ZeroOID, specWP22NewOID)
	a2, m2, err2 := EvaluateGraftPolicy(cfg, "alice", "refs/heads/main", specWP22ZeroOID, specWP22NewOID)
	require.Equal(t, err1, err2)
	require.Equal(t, a1, a2, "same inputs must give the same allowed value")
	require.Equal(t, m1, m2, "same inputs must give the same message")
}
