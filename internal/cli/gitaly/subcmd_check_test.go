package gitaly

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestCheckAPI_Standalone(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	// Unreachable on purpose: in standalone mode `gitaly check` must not contact GitLab.
	cfg.Gitlab.URL = "http://127.0.0.1:1"
	cfg.Gitlab.SecretFile = ""
	cfg.Graft.Standalone = true

	info, err := checkAPI(cfg, testhelper.SharedLogger(t))
	require.NoError(t, err)
	require.Equal(t, "standalone", info.Version)
}
