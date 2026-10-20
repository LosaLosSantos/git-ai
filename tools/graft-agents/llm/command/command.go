// Package command implements an llm.Provider backed by an external program.
//
// The program is configured as an argv (no shell is involved). For every
// completion it receives one JSON object on stdin:
//
//	{"system": "...", "prompt": "...", "json_schema": {...} | null}
//
// and writes the answer text on stdout. When a JSON schema is requested the
// answer is parsed with llm.ExtractJSONObject, so the program may wrap the
// object in prose or a Markdown code fence. A non-zero exit status is an
// error; its stderr (bounded) is included in the error message.
//
// The program inherits graft-resolve's environment, plus GRAFT_LLM_MODEL,
// GRAFT_LLM_EFFORT and GRAFT_LLM_MAX_TOKENS when model, effort and a token
// limit are set (inherited variables of those names are removed). base_url, api_key_env and options are rejected: the program
// reads its own endpoint and credentials.
//
// This backend is the escape hatch for any model or service that has a
// command-line client: the operator decides what runs, Graft only speaks the
// stdin/stdout contract.
package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

func init() {
	llm.Register("command", func(cfg llm.Config) (llm.Provider, error) {
		return New(cfg)
	})
}

const (
	// DefaultMaxOutputBytes bounds what the program may write to stdout.
	DefaultMaxOutputBytes = 8 << 20
	// maxStderrBytes bounds the stderr kept for error messages.
	maxStderrBytes = 4 << 10
	// waitDelay bounds how long I/O is awaited after the program is killed,
	// so a grandchild holding the pipes cannot hang the call.
	waitDelay = 5 * time.Second
)

// ErrOutputTooLarge is returned when the program writes more than the
// configured stdout bound.
var ErrOutputTooLarge = errors.New("command: output exceeds limit")

// ErrNoAnswer is returned when the program produced an empty answer.
var ErrNoAnswer = errors.New("command: empty answer")

// Provider runs an external program per completion. It is stateless and safe
// for concurrent use.
type Provider struct {
	argv           []string
	label          string
	model          string
	effort         string
	maxTokens      int
	timeout        time.Duration
	maxOutputBytes int
}

// Environment variables the program receives, set only when the setting is:
// the model name, the reasoning effort (verbatim), and the answer token limit
// of the call (the request's, else the configured default). The program is
// expected to honour them; the AI budget counts a call's output as at most
// GRAFT_LLM_MAX_TOKENS.
const (
	EnvModel     = "GRAFT_LLM_MODEL"
	EnvEffort    = "GRAFT_LLM_EFFORT"
	EnvMaxTokens = "GRAFT_LLM_MAX_TOKENS"
)

// stdinPayload is the request written to the program's stdin.
type stdinPayload struct {
	System     string         `json:"system"`
	Prompt     string         `json:"prompt"`
	JSONSchema map[string]any `json:"json_schema"`
}

// New builds a command provider from cfg. cfg.Command must name a program.
func New(cfg llm.Config) (*Provider, error) {
	if len(cfg.Command) == 0 {
		return nil, errors.New("command: command is required")
	}
	for i, arg := range cfg.Command {
		if i == 0 && strings.TrimSpace(arg) == "" {
			return nil, errors.New("command: program name is empty")
		}
		if strings.ContainsRune(arg, 0) {
			return nil, fmt.Errorf("command: argument %d contains a NUL byte", i)
		}
	}
	if cfg.MaxTokens < 0 {
		return nil, errors.New("command: max_tokens must not be negative")
	}
	// No setting is dropped silently. The program reads its own endpoint and
	// credentials from its environment, so base_url, api_key_env and options
	// are errors here.
	var unsupported []string
	if strings.TrimSpace(cfg.BaseURL) != "" {
		unsupported = append(unsupported, "base_url")
	}
	if strings.TrimSpace(cfg.APIKeyEnv) != "" {
		unsupported = append(unsupported, "api_key_env")
	}
	if len(cfg.Options) > 0 {
		keys := make([]string, 0, len(cfg.Options))
		for k := range cfg.Options {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		unsupported = append(unsupported, fmt.Sprintf("options %v", keys))
	}
	if cfg.Routing != nil {
		unsupported = append(unsupported, "routing")
	}
	if len(unsupported) > 0 {
		return nil, fmt.Errorf("command: unsupported settings %s: the program reads its endpoint and credentials from its own environment", strings.Join(unsupported, ", "))
	}
	for name, v := range map[string]string{"model": cfg.Model, "effort": cfg.Effort} {
		if strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("command: %s contains a NUL byte", name)
		}
	}
	timeout, err := cfg.TimeoutOrDefault()
	if err != nil {
		return nil, err
	}

	argv := append([]string(nil), cfg.Command...)
	return &Provider{
		argv:           argv,
		label:          "command:" + filepath.Base(argv[0]),
		model:          strings.TrimSpace(cfg.Model),
		effort:         strings.TrimSpace(cfg.Effort),
		maxTokens:      cfg.MaxTokens,
		timeout:        timeout,
		maxOutputBytes: DefaultMaxOutputBytes,
	}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string {
	return p.label
}

// Complete implements llm.Provider. It runs the program once.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("command: prompt is required")
	}
	if req.MaxTokens < 0 {
		return nil, errors.New("command: max_tokens must not be negative")
	}
	if req.JSONSchema != nil && len(req.JSONSchema) == 0 {
		return nil, errors.New("command: json schema must not be empty")
	}

	stdin, err := json.Marshal(stdinPayload{
		System:     req.System,
		Prompt:     req.Prompt,
		JSONSchema: req.JSONSchema,
	})
	if err != nil {
		return nil, fmt.Errorf("command: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	stdout := &limitedBuffer{limit: p.maxOutputBytes}
	stderr := &limitedBuffer{limit: maxStderrBytes}

	cmd := exec.CommandContext(ctx, p.argv[0], p.argv[1:]...)
	// The GRAFT_LLM_ variables are the provider's settings only: inherited ones are removed.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k == EnvModel || k == EnvEffort || k == EnvMaxTokens {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	if p.model != "" {
		cmd.Env = append(cmd.Env, EnvModel+"="+p.model)
	}
	if p.effort != "" {
		cmd.Env = append(cmd.Env, EnvEffort+"="+p.effort)
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.maxTokens
	}
	if maxTokens > 0 {
		cmd.Env = append(cmd.Env, EnvMaxTokens+"="+strconv.Itoa(maxTokens))
	}
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("command: %s: %w", p.label, ctxErr)
		}
		if stdout.exceeded {
			return nil, fmt.Errorf("%w (%d bytes)", ErrOutputTooLarge, p.maxOutputBytes)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("command: %s exited with %d: %s", p.label, exitErr.ExitCode(), strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("command: run %s: %w", p.label, err)
	}
	if stdout.exceeded {
		return nil, fmt.Errorf("%w (%d bytes)", ErrOutputTooLarge, p.maxOutputBytes)
	}

	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return nil, ErrNoAnswer
	}

	resp := &llm.Response{Text: text, Model: p.model}
	if req.JSONSchema != nil {
		raw, err := llm.ExtractJSONObject(text)
		if err != nil {
			return nil, fmt.Errorf("command: %s answer is not a JSON object: %w", p.label, err)
		}
		resp.JSON = raw
	}
	return resp, nil
}

// limitedBuffer keeps at most limit bytes and records whether more were
// written. It never returns an error to the writer, so the program is not
// killed by a broken pipe; the caller decides based on exceeded.
type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if room >= len(p) {
		b.buf.Write(p)
		return len(p), nil
	}
	if room > 0 {
		b.buf.Write(p[:room])
	}
	b.exceeded = true
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}
