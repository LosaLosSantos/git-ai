package hook

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNotAllowedError_StandaloneMessageHasNoGitLabPrefix checks that a denial of
// the standalone policy is reported without the "GitLab: " prefix, which only
// GitLab's access checks carry.
func TestNotAllowedError_StandaloneMessageHasNoGitLabPrefix(t *testing.T) {
	t.Parallel()

	const message = `policy denies "builder" to update "refs/heads/main" (rule 2)`

	require.Equal(t, message, NotAllowedError{Message: message, Standalone: true}.Error())
	require.Equal(t, "GitLab: "+message, NotAllowedError{Message: message}.Error())
}
