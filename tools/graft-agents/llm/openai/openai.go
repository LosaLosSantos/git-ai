// Package openai implements an llm.Provider for any OpenAI-compatible Chat
// Completions endpoint: OpenAI itself, Ollama, vLLM, the llama.cpp server,
// LM Studio, OpenRouter, Mistral and similar. It speaks plain HTTP (no SDK).
//
// Structured output is requested with response_format. When the server
// rejects a format with HTTP 400 the call retries with a weaker format:
// json_schema, then json_object, then plain text with the schema described in
// the system prompt and the object extracted with llm.ExtractJSONObject. Once
// a weaker format has been accepted, the provider keeps using it for the rest
// of its lifetime. A 400 that every format gets (an oversized prompt, a bad
// model name) does not change the format of later calls.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

func init() {
	llm.Register("openai", func(cfg llm.Config) (llm.Provider, error) {
		return New(cfg)
	})
}

const (
	// DefaultBaseURL is the endpoint used when Config.BaseURL is empty.
	DefaultBaseURL = "https://api.openai.com/v1"
	// DefaultAPIKeyEnv is the variable read for the API key when
	// Config.APIKeyEnv is empty.
	DefaultAPIKeyEnv = "OPENAI_API_KEY"
	// MaxRetries is the number of retries after a 429 or 5xx answer.
	MaxRetries = 3

	defaultMaxResponseBytes = 16 << 20
	maxErrorBodyBytes       = 1 << 10
	maxRetryAfter           = 30 * time.Second
	baseBackoff             = 500 * time.Millisecond
	maxBackoff              = 8 * time.Second
	defaultSchemaName       = "graft_answer"
)

// Response format levels, in fallback order.
const (
	levelJSONSchema int32 = iota
	levelJSONObject
	levelText
)

var schemaNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var (
	// ErrRefused is returned when the model refused to answer.
	ErrRefused = errors.New("openai: answer refused")
	// ErrTruncated is returned when the answer hit the token limit.
	ErrTruncated = errors.New("openai: answer truncated by max_tokens")
	// ErrNoAnswer is returned when the answer has no content.
	ErrNoAnswer = errors.New("openai: empty answer")
)

// APIError is a non-2xx answer from the endpoint after retries.
type APIError struct {
	StatusCode int
	// Message is the start of the response body, bounded.
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openai: HTTP %d: %s", e.StatusCode, e.Message)
}

// Provider is an OpenAI-compatible Chat Completions client. It is safe for
// concurrent use.
type Provider struct {
	endpoint         string
	apiKey           string
	model            string
	maxTokens        int
	effort           string
	timeout          time.Duration
	client           *http.Client
	maxResponseBytes int64
	backoff          func(attempt int) time.Duration

	// level is the response_format level in use; it only moves forward, and
	// only to a level the server has accepted.
	level atomic.Int32
}

// New builds a provider from cfg. cfg.Model is required; the API key is
// optional (local servers need none).
func New(cfg llm.Config) (*Provider, error) {
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, errors.New("openai: model is required")
	}
	if cfg.MaxTokens < 0 {
		return nil, errors.New("openai: max_tokens must not be negative")
	}
	// No setting is dropped silently: the provider supports no options, and an
	// effort it cannot send is an error.
	if len(cfg.Options) > 0 {
		keys := make([]string, 0, len(cfg.Options))
		for k := range cfg.Options {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("openai: unsupported options %v: this provider takes none", keys)
	}
	effort := strings.ToLower(strings.TrimSpace(cfg.Effort))
	if effort != "" && !efforts[effort] {
		return nil, fmt.Errorf("openai: invalid effort %q (want minimal, low, medium, high or xhigh)", cfg.Effort)
	}
	timeout, err := cfg.TimeoutOrDefault()
	if err != nil {
		return nil, err
	}

	endpoint, err := chatEndpoint(cfg.BaseURL)
	if err != nil {
		return nil, err
	}

	keyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if keyEnv == "" {
		keyEnv = DefaultAPIKeyEnv
	}
	if strings.ContainsAny(keyEnv, "=\x00") {
		return nil, fmt.Errorf("openai: invalid api_key_env %q", keyEnv)
	}

	return &Provider{
		endpoint:         endpoint,
		apiKey:           os.Getenv(keyEnv),
		model:            model,
		maxTokens:        cfg.MaxTokens,
		effort:           effort,
		timeout:          timeout,
		client:           &http.Client{},
		maxResponseBytes: defaultMaxResponseBytes,
		backoff:          defaultBackoff,
	}, nil
}

// chatEndpoint validates the base URL and returns the Chat Completions URL.
func chatEndpoint(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("openai: invalid base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("openai: base_url must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("openai: base_url has no host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("openai: base_url must not contain a query or fragment")
	}
	return strings.TrimRight(u.String(), "/") + "/chat/completions", nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string {
	return "openai:" + p.model
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("openai: prompt is required")
	}
	if req.MaxTokens < 0 {
		return nil, errors.New("openai: max_tokens must not be negative")
	}
	if req.JSONSchema != nil && len(req.JSONSchema) == 0 {
		return nil, errors.New("openai: json schema must not be empty")
	}
	name := req.SchemaName
	if name == "" {
		name = defaultSchemaName
	}
	if req.JSONSchema != nil && !schemaNameRE.MatchString(name) {
		return nil, fmt.Errorf("openai: invalid schema name %q", name)
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.maxTokens
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	if req.JSONSchema == nil {
		chat, err := p.send(ctx, p.body(req.System, req.Prompt, maxTokens, nil))
		if err != nil {
			return nil, err
		}
		return p.answer(chat, false)
	}

	schemaJSON, err := json.Marshal(req.JSONSchema)
	if err != nil {
		return nil, fmt.Errorf("openai: encode schema: %w", err)
	}

	start := p.level.Load()
	for lvl := start; ; lvl++ {
		system := req.System
		if lvl >= levelJSONObject {
			system = appendSchemaInstruction(system, schemaJSON)
		}
		chat, err := p.send(ctx, p.body(system, req.Prompt, maxTokens, responseFormat(lvl, name, req.JSONSchema)))
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest && lvl < levelText {
			// A 400 may or may not be about response_format (it can also be
			// an oversized prompt, a bad model name, ...). Try the weaker
			// format for this call only; the shared level moves only once
			// a weaker format has actually been accepted.
			continue
		}
		if err != nil {
			return nil, err
		}
		if lvl > start {
			p.raiseLevel(lvl)
		}
		return p.answer(chat, true)
	}
}

// raiseLevel remembers lvl as the response_format level for later calls. The
// level only moves forward; a concurrent call may already have moved it
// further.
func (p *Provider) raiseLevel(lvl int32) {
	for {
		cur := p.level.Load()
		if cur >= lvl || p.level.CompareAndSwap(cur, lvl) {
			return
		}
	}
}

// responseFormat returns the response_format value for a level, or nil for
// plain text.
func responseFormat(lvl int32, name string, schema map[string]any) any {
	switch lvl {
	case levelJSONSchema:
		return map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   name,
				"schema": schema,
				"strict": true,
			},
		}
	case levelJSONObject:
		return map[string]any{"type": "json_object"}
	default:
		return nil
	}
}

func appendSchemaInstruction(system string, schemaJSON []byte) string {
	instruction := "Respond only with a single JSON object that validates against this JSON Schema. " +
		"Do not write any text outside the JSON object.\nJSON Schema:\n" + string(schemaJSON)
	if system == "" {
		return instruction
	}
	return system + "\n\n" + instruction
}

// efforts are the reasoning efforts the provider sends.
var efforts = map[string]bool{"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true}

type chatRequest struct {
	Model          string     `json:"model"`
	Messages       []message  `json:"messages"`
	MaxTokens      int        `json:"max_tokens,omitempty"`
	ResponseFormat any        `json:"response_format,omitempty"`
	Reasoning      *reasoning `json:"reasoning,omitempty"`
}

// reasoning is OpenRouter's reasoning parameter, sent when an effort is
// configured. An endpoint that does not know it may reject the request, so
// configure an effort only for an endpoint that takes it.
type reasoning struct {
	Effort string `json:"effort"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (p *Provider) body(system, prompt string, maxTokens int, format any) []byte {
	req := chatRequest{
		Model:          p.model,
		MaxTokens:      maxTokens,
		ResponseFormat: format,
	}
	if p.effort != "" {
		req.Reasoning = &reasoning{Effort: p.effort}
	}
	if system != "" {
		req.Messages = append(req.Messages, message{Role: "system", Content: system})
	}
	req.Messages = append(req.Messages, message{Role: "user", Content: prompt})

	// Marshalling plain maps and strings cannot fail.
	b, _ := json.Marshal(req)
	return b
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
			Refusal *string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// answer turns a parsed response into an llm.Response.
func (p *Provider) answer(chat *chatResponse, wantJSON bool) (*llm.Response, error) {
	if len(chat.Choices) == 0 {
		return nil, errors.New("openai: response has no choices")
	}
	choice := chat.Choices[0]

	if choice.Message.Refusal != nil && strings.TrimSpace(*choice.Message.Refusal) != "" {
		return nil, fmt.Errorf("%w: %s", ErrRefused, strings.TrimSpace(*choice.Message.Refusal))
	}
	switch choice.FinishReason {
	case "content_filter":
		return nil, fmt.Errorf("%w: blocked by content filter", ErrRefused)
	case "length":
		return nil, ErrTruncated
	}

	text := ""
	if choice.Message.Content != nil {
		text = strings.TrimSpace(*choice.Message.Content)
	}
	if text == "" {
		return nil, ErrNoAnswer
	}

	model := chat.Model
	if model == "" {
		model = p.model
	}
	resp := &llm.Response{
		Text:  text,
		Model: model,
		Usage: llm.Usage{
			InputTokens:  chat.Usage.PromptTokens,
			OutputTokens: chat.Usage.CompletionTokens,
		},
	}
	if wantJSON {
		raw, err := llm.ExtractJSONObject(text)
		if err != nil {
			return nil, fmt.Errorf("openai: answer is not a JSON object: %w", err)
		}
		resp.JSON = raw
	}
	return resp, nil
}

// send posts body, retrying 429 and 5xx answers with backoff, and parses a
// successful answer.
func (p *Provider) send(ctx context.Context, body []byte) (*chatResponse, error) {
	for attempt := 0; ; attempt++ {
		status, header, data, err := p.post(ctx, body)
		if err != nil {
			return nil, err
		}

		if status >= 200 && status < 300 {
			var chat chatResponse
			if err := json.Unmarshal(data, &chat); err != nil {
				return nil, fmt.Errorf("openai: invalid response JSON: %w", err)
			}
			return &chat, nil
		}

		retryable := status == http.StatusTooManyRequests || status >= 500
		if !retryable || attempt >= MaxRetries {
			return nil, &APIError{StatusCode: status, Message: truncate(string(data), maxErrorBodyBytes)}
		}

		wait := parseRetryAfter(header.Get("Retry-After"))
		if wait <= 0 {
			wait = p.backoff(attempt)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *Provider) post(ctx context.Context, body []byte) (int, http.Header, []byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("openai: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, p.maxResponseBytes+1))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("openai: read response: %w", err)
	}
	if int64(len(data)) > p.maxResponseBytes {
		return 0, nil, nil, fmt.Errorf("openai: response exceeds %d bytes", p.maxResponseBytes)
	}
	return resp.StatusCode, resp.Header, data, nil
}

// parseRetryAfter parses a Retry-After header given in seconds. It returns 0
// when absent or unusable, and caps the wait at maxRetryAfter.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}

func defaultBackoff(attempt int) time.Duration {
	d := baseBackoff << attempt
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return strings.ToValidUTF8(s, "")
	}
	return strings.ToValidUTF8(s[:n], "")
}
