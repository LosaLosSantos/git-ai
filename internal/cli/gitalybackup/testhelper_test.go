package gitalybackup

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

func runGitalyBackup(tb testing.TB, ctx context.Context, cfg config.Cfg, stdin io.Reader, args ...string) (string, string, int) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, testcfg.BuildGitalyBackup(tb, cfg), args...)
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		require.ErrorAs(tb, err, &exitErr)
		exitCode = exitErr.ExitCode()
	}

	return stdout.String(), stderr.String(), exitCode
}
