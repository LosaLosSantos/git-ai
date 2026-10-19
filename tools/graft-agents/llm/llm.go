// Package llm is the vendor-neutral language-model abstraction used by the
// Graft agents (graft-resolve, graft-superfan). Graft never depends on one
// vendor: every backend implements Provider and is selected by configuration.
//
// Backends live in sub-packages and register themselves in init():
//
//   - llm/anthropic: Anthropic Messages API through the official Go SDK.
//   - llm/openai: any OpenAI-compatible Chat Completions endpoint (OpenAI,
//     Ollama, vLLM, llama.cpp server, LM Studio, OpenRouter, Mistral, ...).
//   - llm/command: an external program (prompt on stdin, answer on stdout).
//
// Programs import the backends they want with blank imports and build a
// provider with New(Config).
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Request is one completion request.
type Request struct {
	// System is the system prompt (instructions). May be empty.
	System string
	// Prompt is the user message.
	Prompt string
	// JSONSchema, if set, asks the provider to return a JSON object matching
	// this JSON Schema. Providers use their native structured-output feature
	// when available and otherwise instruct the model and extract the JSON.
	// Response.JSON is then always a valid JSON object (validated for
	// syntax; callers validate semantics).
	JSONSchema map[string]any
	// SchemaName names the schema (some providers require a name).
	SchemaName string
	// MaxTokens bounds the answer length; 0 means the provider default.
	MaxTokens int
}

// Usage reports token usage when the provider exposes it.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response is one completion.
type Response struct {
	// Text is the raw text answer.
	Text string
	// JSON is the structured answer when Request.JSONSchema was set.
	JSON json.RawMessage
	// Model is the model that produced the answer, as reported by the
	// provider.
	Model string
	// Usage is the token usage, if known.
	Usage Usage
}

// Provider is a language-model backend.
type Provider interface {
	// Name identifies the backend and model, e.g. "anthropic:claude-opus-5-5"
	// or "openai:llama3.1". It is recorded in commit trailers and logs.
	Name() string
	// Complete runs one completion. It honours ctx cancellation and
	// deadlines.
	Complete(ctx context.Context, req Request) (*Response, error)
}

// Config selects and configures a provider. It is embedded in the TOML
// configuration of the Graft agents.
//
// A provider honours every field it is given or fails at construction: no
// setting is dropped silently.
type Config struct {
	// Type is "anthropic", "openai", "command" or "none".
	Type string `toml:"type" json:"type"`
	// Model is the model identifier (provider-specific).
	Model string `toml:"model" json:"model"`
	// BaseURL overrides the API endpoint (openai: required for non-OpenAI
	// servers, e.g. "http://localhost:11434/v1" for Ollama).
	BaseURL string `toml:"base_url" json:"base_url"`
	// APIKeyEnv names the environment variable holding the API key. Empty
	// means the provider's conventional variable (or none for local
	// servers).
	APIKeyEnv string `toml:"api_key_env" json:"api_key_env"`
	// Command is the program and arguments for the "command" type.
	Command []string `toml:"command" json:"command"`
	// Timeout bounds one completion (Go duration, e.g. "300s"); empty means
	// DefaultTimeout.
	Timeout string `toml:"timeout" json:"timeout"`
	// MaxTokens is the default Request.MaxTokens.
	MaxTokens int `toml:"max_tokens" json:"max_tokens"`
	// Effort is a provider-specific reasoning effort ("low", "medium",
	// "high", ...); a provider that cannot send it rejects it.
	Effort string `toml:"effort" json:"effort"`
	// Options holds extra provider-specific settings.
	Options map[string]string `toml:"options" json:"options"`
	// Routing pins the OpenRouter endpoints a call may use (openai provider only).
	Routing *Routing `toml:"routing" json:"routing"`
}

// DefaultTimeout is the default completion timeout.
const DefaultTimeout = 300 * time.Second

// TimeoutOrDefault parses Timeout.
func (c Config) TimeoutOrDefault() (time.Duration, error) {
	if c.Timeout == "" {
		return DefaultTimeout, nil
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("llm: invalid timeout %q", c.Timeout)
	}
	return d, nil
}

// Factory builds a provider from its configuration.
type Factory func(Config) (Provider, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes a backend available under typ. It panics on duplicates.
func Register(typ string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := factories[typ]; ok {
		panic("llm: duplicate provider type " + typ)
	}
	factories[typ] = f
}

// Types returns the registered provider types, sorted.
func Types() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for t := range factories {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ErrDisabled is returned by New for Type "none" (or empty).
var ErrDisabled = errors.New("llm: provider disabled")

// New builds the provider selected by cfg.Type.
func New(cfg Config) (Provider, error) {
	typ := strings.TrimSpace(cfg.Type)
	if typ == "" || typ == "none" {
		return nil, ErrDisabled
	}
	mu.RLock()
	f, ok := factories[typ]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: unknown provider type %q (available: %s)", typ, strings.Join(Types(), ", "))
	}
	return f(cfg)
}

// ExtractJSONObject returns the first complete top-level JSON object found
// in text (ignoring Markdown code fences and surrounding prose). Backends
// without native structured output use it.
func ExtractJSONObject(text string) (json.RawMessage, error) {
	for start := strings.IndexByte(text, '{'); start >= 0; {
		dec := json.NewDecoder(strings.NewReader(text[start:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil {
			var obj map[string]any
			if json.Unmarshal(raw, &obj) == nil {
				return raw, nil
			}
		}
		next := strings.IndexByte(text[start+1:], '{')
		if next < 0 {
			break
		}
		start += 1 + next
	}
	return nil, errors.New("llm: no JSON object in answer")
}
