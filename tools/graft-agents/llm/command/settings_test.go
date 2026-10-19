package command

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

func helperEnvOf(t *testing.T, cfg llm.Config, req llm.Request) map[string]string {
	t.Helper()
	resp, err := newProvider(t, cfg).Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(resp.Text), &env); err != nil {
		t.Fatalf("helper output %q: %v", resp.Text, err)
	}
	return env
}

func TestSettingsReachTheProgramAsEnvironment(t *testing.T) {
	cfg := helperConfig(t, "env")
	cfg.Effort = " low "
	cfg.MaxTokens = 900

	env := helperEnvOf(t, cfg, llm.Request{Prompt: "hi"})
	want := map[string]string{EnvModel: "helper-model", EnvEffort: "low", EnvMaxTokens: "900"}
	if len(env) != len(want) {
		t.Fatalf("env %v, want %v", env, want)
	}
	for k, v := range want {
		if env[k] != v {
			t.Fatalf("env %v, want %v", env, want)
		}
	}

	// The request's limit takes precedence over the configured default.
	if env := helperEnvOf(t, cfg, llm.Request{Prompt: "hi", MaxTokens: 50}); env[EnvMaxTokens] != "50" {
		t.Fatalf("max tokens = %q, want the request's 50", env[EnvMaxTokens])
	}
}

func TestUnsetSettingsAreNotInTheEnvironment(t *testing.T) {
	// Values inherited from graft-resolve's own environment are removed: the variables carry
	// the provider's settings and nothing else.
	t.Setenv(EnvEffort, "inherited")
	t.Setenv(EnvMaxTokens, "123")
	cfg := helperConfig(t, "env")
	cfg.Model = ""
	env := helperEnvOf(t, cfg, llm.Request{Prompt: "hi"})
	if len(env) != 0 {
		t.Fatalf("env %v, want no GRAFT_LLM_ variables", env)
	}
}

func TestNewRejectsSettingsTheProgramCannotTake(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*llm.Config)
		want   string
	}{
		"base_url":    {func(c *llm.Config) { c.BaseURL = "https://example.com/v1" }, "base_url"},
		"routing":     {func(c *llm.Config) { c.Routing = &llm.Routing{Only: []string{"openai"}} }, "routing"},
		"api_key_env": {func(c *llm.Config) { c.APIKeyEnv = "KEY" }, "api_key_env"},
		"options":     {func(c *llm.Config) { c.Options = map[string]string{"b": "1", "a": "2"} }, "options [a b]"},
		"nul effort":  {func(c *llm.Config) { c.Effort = "lo\x00w" }, "effort contains a NUL byte"},
	} {
		cfg := helperConfig(t, "env")
		tc.mutate(&cfg)
		_, err := New(cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
