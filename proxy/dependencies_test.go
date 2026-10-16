package proxy

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// The proxy package is imported by external modules, which must list every module in its dependency closure in their
// own go.mod. This test guards against sentry-go entering that closure through an internal package.
func TestProxy_doesNotDependOnSentry(t *testing.T) {
	t.Parallel()

	output, err := exec.CommandContext(testhelper.Context(t), "go", "list", "-deps", "github.com/feder-cr/git-ai/proxy").Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		require.FailNow(t, "go list failed", "%v: %s", err, exitErr.Stderr)
	}
	require.NoError(t, err)

	var sentryDependencies []string
	for _, dependency := range strings.Fields(string(output)) {
		if strings.HasPrefix(dependency, "github.com/getsentry/sentry-go") {
			sentryDependencies = append(sentryDependencies, dependency)
		}
	}
	require.Empty(t, sentryDependencies, "proxy must not depend on sentry-go, but imports it via an internal package")
}
