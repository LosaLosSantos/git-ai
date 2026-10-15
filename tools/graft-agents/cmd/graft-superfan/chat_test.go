package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunChatUsageErrors(t *testing.T) {
	tests := map[string][]string{
		"no command":           {"chat"},
		"unknown command":      {"chat", "bogus"},
		"serve without config": {"chat", "serve"},
		"ask without question": {"chat", "ask"},
		"ask unknown flag":     {"chat", "ask", "--nope", "q"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run(context.Background(), args, nil, &out, &errb); code != exitUsage {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, exitUsage, errb.String())
			}
		})
	}
}

func TestRunChatAskAnswersFromSources(t *testing.T) {
	state := filepath.Join(t.TempDir(), "verify.txt")
	if err := os.WriteFile(state, []byte("status: green\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgData, err := json.Marshal(map[string]any{
		"sources":  map[string]any{"verify_state": state},
		"provider": map[string]any{"type": "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(cfgPath, cfgData, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	args := []string{"chat", "ask", "--config", cfgPath, "what", "is", "the", "status?"}
	if code := run(context.Background(), args, nil, &out, &errb); code != exitOK {
		t.Fatalf("exit = %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "green") {
		t.Fatalf("answer = %q", out.String())
	}
}

func TestRunChatAskWithoutConfig(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"chat", "ask", "anything?"}, nil, &out, &errb)
	if code != exitOK || !strings.Contains(out.String(), "No sources are configured") {
		t.Fatalf("exit = %d, out %q, stderr %q", code, out.String(), errb.String())
	}
}

func TestRunChatBadConfigIsAnError(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(cfgPath, []byte(`{"sorces":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"chat", "ask", "--config", cfgPath, "q"}, nil, &out, &errb); code != exitError {
		t.Fatalf("ask: exit = %d, want %d", code, exitError)
	}
	if code := run(context.Background(), []string{"chat", "serve", "--config", cfgPath}, nil, &out, &errb); code != exitError {
		t.Fatalf("serve: exit = %d, want %d", code, exitError)
	}
}

func TestRunChatServeNeedsSigningSecret(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRAFT_SUPERFAN_SLACK_SIGNING_SECRET", "")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"chat", "serve", "--config", cfgPath}, nil, &out, &errb)
	if code != exitError || !strings.Contains(errb.String(), "GRAFT_SUPERFAN_SLACK_SIGNING_SECRET") {
		t.Fatalf("exit = %d, stderr %q", code, errb.String())
	}
}
