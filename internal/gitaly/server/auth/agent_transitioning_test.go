package auth

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	gitalycfgauth "github.com/feder-cr/open_scion/internal/gitaly/config/auth"
	"github.com/stretchr/testify/require"
)

// TestInterceptors_TransitioningWithAgentsFailsAtStartup checks that transitioning
// mode is refused when agents are configured. In transitioning mode a request
// that fails authentication has no identity, so a request with a wrong agent
// token would skip the allowlist. Without agents, transitioning mode is unchanged.
func TestInterceptors_TransitioningWithAgentsFailsAtStartup(t *testing.T) {
	t.Parallel()

	agents := []config.GraftAgent{{Name: "a", Token: "agent-secret"}}
	for _, external := range []bool{true, false} {
		_, _, err := Interceptors(gitalycfgauth.Config{Token: "shared", Transitioning: true}, agents, external)
		require.ErrorContains(t, err, "auth.transitioning must be false")
	}

	_, _, err := Interceptors(gitalycfgauth.Config{Token: "shared", Transitioning: true}, nil, true)
	require.NoError(t, err)
}
