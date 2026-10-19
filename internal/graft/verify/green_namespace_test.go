package verify

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/agentrefs"
)

// TestGreenRefPrefix_isAServerNamespace: the green references' namespace is one agentrefs lists,
// so that agents do not see it, nor reach it by its short form.
func TestGreenRefPrefix_isAServerNamespace(t *testing.T) {
	t.Parallel()

	var listed []string
	for _, ns := range agentrefs.ServerNamespaces {
		listed = append(listed, agentrefs.HiddenPrefix+ns+"/")
	}
	require.Contains(t, listed, greenRefPrefix)
}
