package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// validConfigTOML is a complete configuration that every test case extends.
const validConfigTOML = `
address = "unix:/run/gitaly.sock"
repository = "project.git"
state_file = "/var/lib/graft-verify/state.json"
work_dir = "/var/lib/graft-verify/work"
test = { command = ["go", "test", "./..."] }
`

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(validConfigTOML))
	require.NoError(t, err)

	require.Equal(t, DefaultStorage, cfg.Storage)
	require.Equal(t, DefaultBranch, cfg.Branch)
	require.Equal(t, "refs/graft/green/main", cfg.GreenRef)
	require.Equal(t, Duration(DefaultBatchWindow), cfg.BatchWindow)
	require.Equal(t, Duration(DefaultTestTimeout), cfg.Test.Timeout)
	require.Empty(t, cfg.Sparse)
	require.False(t, cfg.Revert.Enabled)
}

func TestParseConfigFull(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
address = "tcp://gitaly.internal:8075"
token = "secret"
storage = "shards"
repository = "group/project.git"
branch = "release/1.0"
green_ref = "refs/graft/green/release-1.0"
work_dir = "/var/lib/graft-verify/work"
state_file = "/var/lib/graft-verify/state.json"
sparse = ["cmd/", "internal/"]
batch_window = "45s"

[test]
command = ["make", "test"]
env = ["CI=1", "GOFLAGS=-mod=mod"]
timeout = "5m"

[revert]
enabled = true
dry_run = true
max_per_hour = 3
webhook_url = "https://hooks.example.test/x"
`))
	require.NoError(t, err)

	require.Equal(t, "secret", cfg.Token)
	require.Equal(t, "shards", cfg.Storage)
	require.Equal(t, "release/1.0", cfg.Branch)
	require.Equal(t, "refs/graft/green/release-1.0", cfg.GreenRef)
	require.Equal(t, []string{"cmd/", "internal/"}, cfg.Sparse)
	require.Equal(t, 45*time.Second, cfg.BatchWindow.Std())
	require.Equal(t, []string{"make", "test"}, cfg.Test.Command)
	require.Equal(t, []string{"CI=1", "GOFLAGS=-mod=mod"}, cfg.Test.Env)
	require.Equal(t, 5*time.Minute, cfg.Test.Timeout.Std())
	require.Equal(t, RevertConfig{
		Enabled:    true,
		DryRun:     true,
		MaxPerHour: 3,
		WebhookURL: "https://hooks.example.test/x",
	}, cfg.Revert)
}

func TestParseConfigRejects(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			name:    "unknown key",
			toml:    validConfigTOML + "\nbranchh = \"main\"\n",
			wantErr: "strict mode",
		},
		{
			name:    "missing address",
			toml:    strings.Replace(validConfigTOML, `address = "unix:/run/gitaly.sock"`, "", 1),
			wantErr: "address is required",
		},
		{
			name:    "missing repository",
			toml:    strings.Replace(validConfigTOML, `repository = "project.git"`, "", 1),
			wantErr: "repository is required",
		},
		{
			name:    "missing state file",
			toml:    strings.Replace(validConfigTOML, `state_file = "/var/lib/graft-verify/state.json"`, "", 1),
			wantErr: "state_file is required",
		},
		{
			name:    "missing work dir",
			toml:    strings.Replace(validConfigTOML, `work_dir = "/var/lib/graft-verify/work"`, "", 1),
			wantErr: "work_dir is required",
		},
		{
			name:    "missing test command",
			toml:    strings.Replace(validConfigTOML, `command = ["go", "test", "./..."]`, `command = []`, 1),
			wantErr: "test.command is required",
		},
		{
			name:    "empty test command program",
			toml:    strings.Replace(validConfigTOML, `command = ["go", "test", "./..."]`, `command = [""]`, 1),
			wantErr: "test.command is required",
		},
		{
			name:    "token and token file",
			toml:    validConfigTOML + "\ntoken = \"a\"\ntoken_file = \"/etc/token\"\n",
			wantErr: "mutually exclusive",
		},
		{
			name:    "branch with dots",
			toml:    validConfigTOML + "\nbranch = \"a..b\"\n",
			wantErr: "not a valid branch name",
		},
		{
			name:    "branch with leading dash",
			toml:    validConfigTOML + "\nbranch = \"-main\"\n",
			wantErr: "not a valid branch name",
		},
		{
			name:    "branch with space",
			toml:    validConfigTOML + "\nbranch = \"a b\"\n",
			wantErr: "not a valid branch name",
		},
		{
			name:    "branch with lock suffix",
			toml:    validConfigTOML + "\nbranch = \"main.lock\"\n",
			wantErr: "not a valid branch name",
		},
		{
			name:    "green ref outside the green namespace",
			toml:    validConfigTOML + "\ngreen_ref = \"refs/heads/main\"\n",
			wantErr: "must be a valid reference under refs/graft/green/",
		},
		{
			name:    "green ref without a name",
			toml:    validConfigTOML + "\ngreen_ref = \"refs/graft/green/\"\n",
			wantErr: "must be a valid reference under refs/graft/green/",
		},
		{
			name:    "green ref with an invalid component",
			toml:    validConfigTOML + "\ngreen_ref = \"refs/graft/green/a..b\"\n",
			wantErr: "must be a valid reference under refs/graft/green/",
		},
		{
			name:    "negative batch window",
			toml:    validConfigTOML + "\nbatch_window = \"-1s\"\n",
			wantErr: "batch_window must not be negative",
		},
		{
			name:    "malformed duration",
			toml:    validConfigTOML + "\nbatch_window = \"soon\"\n",
			wantErr: "invalid duration",
		},
		{
			name:    "negative test timeout",
			toml:    strings.Replace(validConfigTOML, `command = ["go", "test", "./..."]`, `command = ["go"], timeout = "-1m"`, 1),
			wantErr: "test.timeout must be positive",
		},
		{
			name:    "env entry without equals sign",
			toml:    strings.Replace(validConfigTOML, `command = ["go", "test", "./..."]`, `command = ["go"], env = ["CI"]`, 1),
			wantErr: "must be KEY=VALUE",
		},
		{
			name:    "env entry with empty key",
			toml:    strings.Replace(validConfigTOML, `command = ["go", "test", "./..."]`, `command = ["go"], env = ["=1"]`, 1),
			wantErr: "must be KEY=VALUE",
		},
		{
			name:    "empty sparse entry",
			toml:    validConfigTOML + "\nsparse = [\"\"]\n",
			wantErr: "sparse entries must not be empty",
		},
		{
			name:    "negative revert cap",
			toml:    validConfigTOML + "\n[revert]\nmax_per_hour = -1\n",
			wantErr: "max_per_hour must not be negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.toml))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestParseConfigAcceptsValidRefNames(t *testing.T) {
	for _, branch := range []string{"main", "release/1.0", "feature-x_y", "a/b/c"} {
		cfg, err := ParseConfig([]byte(validConfigTOML + "\nbranch = \"" + branch + "\"\n"))
		require.NoError(t, err, branch)
		require.Equal(t, "refs/graft/green/"+branch, cfg.GreenRef)
	}
}

func TestFinalizeReadsTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("  shared-secret\n\n"), 0o600))

	cfg, err := ParseConfig([]byte(validConfigTOML + "\ntoken_file = \"" + path + "\"\n"))
	require.NoError(t, err)
	require.Equal(t, "shared-secret", cfg.Token)
}

func TestFinalizeMissingTokenFile(t *testing.T) {
	_, err := ParseConfig([]byte(validConfigTOML + "\ntoken_file = \"/nonexistent/graft-verify-token\"\n"))
	require.ErrorContains(t, err, "read token_file")
}

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graft-verify.toml")
	require.NoError(t, os.WriteFile(path, []byte(validConfigTOML), 0o600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "project.git", cfg.Repository)

	_, err = LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	require.ErrorContains(t, err, "read config")
}

func TestDurationMarshalRoundTrip(t *testing.T) {
	text, err := Duration(90 * time.Second).MarshalText()
	require.NoError(t, err)
	require.Equal(t, "1m30s", string(text))

	var d Duration
	require.NoError(t, d.UnmarshalText(text))
	require.Equal(t, 90*time.Second, d.Std())
}

func TestValidRefName(t *testing.T) {
	valid := []string{"main", "a/b", "v1.2.3", "x-y_z", "release/1.0"}
	invalid := []string{"", "/a", "a/", "a//b", "a..b", "a.", "a b", "a~b", "a^b", "a:b",
		"a?b", "a*b", "a[b", "a\\b", "a@{b", "a.lock", "a/.hidden", "a/b.lock", "a\x01b", "./a", "a/./b"}

	for _, name := range valid {
		require.True(t, validRefName(name), "%q should be valid", name)
	}
	for _, name := range invalid {
		require.False(t, validRefName(name), "%q should be invalid", name)
	}
}
