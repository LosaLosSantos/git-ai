package chat

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte("{}"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := cfg.contextBudget(); got != DefaultMaxContextBytes {
		t.Errorf("contextBudget = %d, want %d", got, DefaultMaxContextBytes)
	}
	if got := cfg.Sources.tailCount(); got != DefaultLogTailLines {
		t.Errorf("tailCount = %d, want %d", got, DefaultLogTailLines)
	}
	if got := cfg.SlackAPIBase(); got != "https://slack.com/api" {
		t.Errorf("SlackAPIBase = %q", got)
	}
	if got := cfg.signingSecretEnv(); got != defaultSigningSecretEnv {
		t.Errorf("signingSecretEnv = %q", got)
	}
	if got := cfg.botTokenEnv(); got != defaultBotTokenEnv {
		t.Errorf("botTokenEnv = %q", got)
	}
}

func TestConfigSlackAPIBaseTrimsSlash(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"slack":{"api_base":"http://127.0.0.1:9/api/"}}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := cfg.SlackAPIBase(); got != "http://127.0.0.1:9/api" {
		t.Fatalf("SlackAPIBase = %q", got)
	}
}

func TestConfigRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	for _, doc := range []string{`{"sorces":{}}`, `{} {}`, ``} {
		if _, err := ParseConfig([]byte(doc)); err == nil {
			t.Errorf("ParseConfig(%q) succeeded, want an error", doc)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{name: "valid", doc: `{"sources":{"changesets":{"argv":["graft","log","--json","-n","50"],"dir":"/w"},"verify_state":"/v.json","metrics_url":"http://127.0.0.1:9090/metrics","log_files":["/l.log"],"log_tail_lines":100},"max_context_bytes":4096,"redact":["ticket-[0-9]+"],"provider":{"type":"none"}}`},
		{name: "argv and file", doc: `{"sources":{"changesets":{"argv":["graft"],"file":"/c.json"}}}`, wantErr: "exclusive"},
		{name: "dir without argv", doc: `{"sources":{"changesets":{"dir":"/w"}}}`, wantErr: "needs argv"},
		{name: "empty program", doc: `{"sources":{"changesets":{"argv":[""]}}}`, wantErr: "empty program"},
		{name: "bad metrics URL", doc: `{"sources":{"metrics_url":"file:///etc/passwd"}}`, wantErr: "metrics_url"},
		{name: "bad slack API base", doc: `{"slack":{"api_base":"slack.example"}}`, wantErr: "api_base"},
		{name: "too many log lines", doc: `{"sources":{"log_tail_lines":5000}}`, wantErr: "log_tail_lines"},
		{name: "empty log path", doc: `{"sources":{"log_files":[""]}}`, wantErr: "empty log file"},
		{name: "context too small", doc: `{"max_context_bytes":10}`, wantErr: "max_context_bytes"},
		{name: "negative context", doc: `{"max_context_bytes":-1}`, wantErr: "max_context_bytes"},
		{name: "bad redact pattern", doc: `{"redact":["("]}`, wantErr: "redact pattern"},
		{name: "bad provider timeout", doc: `{"provider":{"type":"none","timeout":"soon"}}`, wantErr: "provider"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.doc))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseConfig: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParseConfig error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("LoadConfig of a missing file succeeded")
	}
}

func TestLoadSlackSecrets(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		defaultSigningSecretEnv: "sign-value",
		defaultBotTokenEnv:      "xoxb-bot-value",
	}
	signing, token, err := LoadSlackSecrets(cfg, func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("LoadSlackSecrets: %v", err)
	}
	if signing != "sign-value" || token != "xoxb-bot-value" {
		t.Fatalf("LoadSlackSecrets = %q, %q", signing, token)
	}

	// The bot token is optional.
	delete(env, defaultBotTokenEnv)
	if _, token, err := LoadSlackSecrets(cfg, func(k string) string { return env[k] }); err != nil || token != "" {
		t.Fatalf("without bot token: token %q, err %v", token, err)
	}

	// The signing secret is required, and the error names the variable only.
	delete(env, defaultSigningSecretEnv)
	_, _, err = LoadSlackSecrets(cfg, func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), defaultSigningSecretEnv) {
		t.Fatalf("missing signing secret: err = %v", err)
	}
}

func TestLoadSlackSecretsUsesConfiguredNames(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"slack":{"signing_secret_env":"MY_SIGN","bot_token_env":"MY_BOT"}}`))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MY_SIGN": "s", "MY_BOT": "b"}
	signing, token, err := LoadSlackSecrets(cfg, func(k string) string { return env[k] })
	if err != nil || signing != "s" || token != "b" {
		t.Fatalf("LoadSlackSecrets = %q, %q, %v", signing, token, err)
	}
}
