package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunRequiresConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	require.Equal(t, exitError, run(nil, &stdout, &stderr))
	require.Contains(t, stderr.String(), "-config is required")
}

func TestRunHelpExitsOK(t *testing.T) {
	var stdout, stderr bytes.Buffer
	require.Equal(t, exitOK, run([]string{"-h"}, &stdout, &stderr))
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	require.Equal(t, exitError, run([]string{"-no-such-flag"}, &stdout, &stderr))
}

func TestRunRejectsMissingConfigFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "missing.toml")
	require.Equal(t, exitError, run([]string{"-config", path}, &stdout, &stderr))
	require.Contains(t, stderr.String(), "read config")
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "graft-verify.toml")
	require.NoError(t, os.WriteFile(path, []byte("repository = \"x.git\"\n"), 0o600))

	require.Equal(t, exitError, run([]string{"-config", path}, &stdout, &stderr))
	require.Contains(t, stderr.String(), "address is required")
}

func TestRunFailsWhenServerIsUnreachable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	path := filepath.Join(dir, "graft-verify.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
address = "unix:`+filepath.Join(dir, "no-such.sock")+`"
repository = "project.git"
work_dir = "`+filepath.Join(dir, "work")+`"
state_file = "`+filepath.Join(dir, "state.json")+`"
test = { command = ["true"] }
`), 0o600))

	// The dial is lazy, so the failure is reported by the first call that
	// reads the green ref, which happens during Open.
	code := run([]string{"-config", path, "-once"}, &stdout, &stderr)
	require.Equal(t, exitError, code)
	require.NotEmpty(t, stderr.String())
}
