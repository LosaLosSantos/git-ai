package superfan

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfigJSON = `{
  "window": "30m",
  "components": [
    {"name": "payments", "paths": ["ledger/**", "api/pay/*.go"]},
    {"name": "docs", "paths": ["**/*.md"]}
  ],
  "routes": [
    {"component": "payments", "notifiers": ["ops-slack", "console"]},
    {"component": "*", "notifiers": ["console"]}
  ],
  "notifiers": [
    {"name": "console", "type": "stdout"},
    {"name": "ops-slack", "type": "slack", "url": "https://hooks.example.com/services/T/B/X", "timeout": "5s"},
    {"name": "audit", "type": "webhook", "url": "https://audit.example.com/hook", "token_file": "/run/secrets/audit"}
  ],
  "rules": {
    "large_change": {"max_files": 50, "max_lines": 2000},
    "protected_paths": ["ledger/**"],
    "burst": {"lands": 10, "window": "5m"},
    "expected_actors": ["agent-*", "ci"]
  },
  "summarizer": {"type": "none"},
  "cursor": "/var/lib/graft/superfan.cursor"
}`

func TestParseConfigValid(t *testing.T) {
	cfg, err := ParseConfig([]byte(validConfigJSON))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.WindowDuration() != 30*time.Minute {
		t.Errorf("window = %s", cfg.WindowDuration())
	}
	if len(cfg.Components) != 2 || len(cfg.Routes) != 2 || len(cfg.Notifiers) != 3 {
		t.Errorf("unexpected shape: %+v", cfg)
	}
	if cfg.Rules.LargeChange == nil || cfg.Rules.LargeChange.MaxFiles != 50 || cfg.Rules.Burst.Lands != 10 {
		t.Errorf("rules not decoded: %+v", cfg.Rules)
	}
}

func TestParseConfigRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		{"unknown field", `{"windw": "1h"}`, "unknown field"},
		{"trailing data", `{} {}`, "trailing data"},
		{"not json", `{`, "superfan config"},
		{"bad window", `{"window": "soon"}`, "invalid window"},
		{"window too small", `{"window": "10ms"}`, "must be between"},
		{"window too large", `{"window": "400h"}`, "must be between"},
		{"component name", `{"components": [{"name": "a b", "paths": ["x"]}]}`, "invalid component name"},
		{"reserved component", `{"components": [{"name": "unassigned", "paths": ["x"]}]}`, "reserved"},
		{"duplicate component", `{"components": [{"name": "a", "paths": ["x"]}, {"name": "a", "paths": ["y"]}]}`, "duplicate component"},
		{"component without paths", `{"components": [{"name": "a", "paths": []}]}`, "has no paths"},
		{"component bad glob", `{"components": [{"name": "a", "paths": ["x//y"]}]}`, "path"},
		{"notifier type", `{"notifiers": [{"name": "n", "type": "email"}]}`, "unknown type"},
		{"duplicate notifier", `{"notifiers": [{"name": "n", "type": "stdout"}, {"name": "n", "type": "stdout"}]}`, "duplicate notifier"},
		{"stdout with url", `{"notifiers": [{"name": "n", "type": "stdout", "url": "https://x"}]}`, "takes no url"},
		{"webhook without url", `{"notifiers": [{"name": "n", "type": "webhook"}]}`, "url is required"},
		{"webhook bad scheme", `{"notifiers": [{"name": "n", "type": "webhook", "url": "gopher://x"}]}`, "http or https"},
		{"slack with token file", `{"notifiers": [{"name": "n", "type": "slack", "url": "https://x", "token_file": "t"}]}`, "takes no token_file"},
		{"bad timeout", `{"notifiers": [{"name": "n", "type": "stdout", "timeout": "-1s"}]}`, "invalid timeout"},
		{"route unknown component", `{"notifiers": [{"name": "n", "type": "stdout"}], "routes": [{"component": "ghost", "notifiers": ["n"]}]}`, "unknown component"},
		{"route unknown notifier", `{"routes": [{"component": "*", "notifiers": ["ghost"]}]}`, "unknown notifier"},
		{"route without notifiers", `{"routes": [{"component": "*", "notifiers": []}]}`, "needs 1 to"},
		{"large change empty", `{"rules": {"large_change": {}}}`, "needs max_files or max_lines"},
		{"large change negative", `{"rules": {"large_change": {"max_files": -1, "max_lines": 5}}}`, "must not be negative"},
		{"protected bad glob", `{"rules": {"protected_paths": ["a/../b"]}}`, "protected path"},
		{"burst lands", `{"rules": {"burst": {"lands": 1, "window": "1m"}}}`, "burst lands"},
		{"burst window", `{"rules": {"burst": {"lands": 3, "window": "forever"}}}`, "invalid burst window"},
		{"expected actor bad glob", `{"rules": {"expected_actors": ["a/["]}}`, "expected actor"},
		{"unknown summarizer", `{"summarizer": {"type": "oracle"}}`, "unknown summarizer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.doc))
			if err == nil {
				t.Fatalf("accepted %s", tc.doc)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q lacks %q", err, tc.want)
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("empty config: %v", err)
	}
	if cfg.WindowDuration() != DefaultWindow {
		t.Errorf("default window = %s", cfg.WindowDuration())
	}
	notifiers, err := cfg.BuildNotifiers(&bytes.Buffer{})
	if err != nil || len(notifiers) != 0 {
		t.Errorf("BuildNotifiers on empty config = %v, %v", notifiers, err)
	}
}

func TestBuildNotifiersReadsTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Notifiers: []NotifierConfig{
		{Name: "console", Type: "stdout"},
		{Name: "audit", Type: "webhook", URL: "https://audit.example.com/hook", TokenFile: tokenPath, Timeout: "3s"},
		{Name: "ops", Type: "slack", URL: "https://hooks.example.com/x"},
	}}
	notifiers, err := cfg.BuildNotifiers(&bytes.Buffer{})
	if err != nil {
		t.Fatalf("BuildNotifiers: %v", err)
	}
	for _, name := range []string{"console", "audit", "ops"} {
		if n := notifiers[name]; n == nil || n.Name() != name {
			t.Errorf("notifier %q missing or misnamed: %v", name, n)
		}
	}
}

func TestBuildNotifiersTokenErrors(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		file string
		want string
	}{
		{"missing file", filepath.Join(dir, "absent"), "cannot read token file"},
		{"empty token", empty, "token file is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Notifiers: []NotifierConfig{
				{Name: "audit", Type: "webhook", URL: "https://audit.example.com/hook", TokenFile: tc.file},
			}}
			_, err := cfg.BuildNotifiers(&bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), tc.file) {
				t.Errorf("error reveals the token path: %v", err)
			}
		})
	}
}

func TestValidateDirectConfigIsStrict(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = append(cfg.Routes, RouteConfig{Component: "payments", Notifiers: []string{"nobody"}})
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an unknown notifier in a route")
	}
	cfg = baseConfig()
	cfg.Window = "0s"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a zero window")
	}
}
