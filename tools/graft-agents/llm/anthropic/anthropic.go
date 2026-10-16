// Package anthropic implements an llm.Provider for the Anthropic Messages API
// through the official Go SDK (github.com/anthropics/anthropic-sdk-go).
//
// Requests use adaptive thinking and output_config.effort (default "high").
// When a JSON schema is requested it is sent as output_config.format, so the
// answer is constrained by the API. Long outputs are streamed to avoid HTTP
// timeouts, and stop_reason is checked before the content is read: a refusal
// or a max_tokens cut-off is an error, never a partial answer.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

func init() {
	llm.Register("anthropic", func(cfg llm.Config) (llm.Provider, error) {
		return New(cfg)
	})
}

const (
	// DefaultModel is used when Config.Model is empty.
	DefaultModel = "claude-opus-5-5"
	// DefaultEffort is used when Config.Effort is empty.
	DefaultEffort = "high"
	// DefaultMaxTokens is used when neither the request nor the config sets
	// a limit.
	DefaultMaxTokens = 16000
	// streamingThreshold is the max_tokens above which the answer is
	// streamed instead of returned in one HTTP response.
	streamingThreshold = 16000
)

var (
	// ErrRefused is returned when the model refused to answer.
	ErrRefused = errors.New("anthropic: answer refused")
	// ErrTruncated is returned when the answer hit max_tokens.
	ErrTruncated = errors.New("anthropic: answer truncated by max_tokens")
	// ErrNoAnswer is returned when the answer has no text.
	ErrNoAnswer = errors.New("anthropic: empty answer")
)

var efforts = map[string]anthropicsdk.OutputConfigEffort{
	"low":    anthropicsdk.OutputConfigEffortLow,
	"medium": anthropicsdk.OutputConfigEffortMedium,
	"high":   anthropicsdk.OutputConfigEffortHigh,
	"xhigh":  anthropicsdk.OutputConfigEffortXhigh,
	"max":    anthropicsdk.OutputConfigEffortMax,
}

// Provider is an Anthropic Messages API client. It is safe for concurrent
// use.
type Provider struct {
	client    anthropicsdk.Client
	model     string
	effort    anthropicsdk.OutputConfigEffort
	maxTokens int64
	timeout   time.Duration
}

// New builds a provider from cfg. The API key comes from the variable named by
// cfg.APIKeyEnv when set (it must be non-empty), and otherwise from the SDK's
// default resolution (ANTHROPIC_API_KEY).
func New(cfg llm.Config) (*Provider, error) {
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = DefaultModel
	}

	effortName := strings.ToLower(strings.TrimSpace(cfg.Effort))
	if effortName == "" {
		effortName = DefaultEffort
	}
	effort, ok := efforts[effortName]
	if !ok {
		return nil, fmt.Errorf("anthropic: invalid effort %q (want low, medium, high, xhigh or max)", cfg.Effort)
	}

	if cfg.MaxTokens < 0 {
		return nil, errors.New("anthropic: max_tokens must not be negative")
	}
	maxTokens := int64(cfg.MaxTokens)
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}

	timeout, err := cfg.TimeoutOrDefault()
	if err != nil {
		return nil, err
	}

	var opts []option.RequestOption
	if base := strings.TrimSpace(cfg.BaseURL); base != "" {
		if err := validateBaseURL(base); err != nil {
			return nil, err
		}
		opts = append(opts, option.WithBaseURL(base))
	}
	if keyEnv := strings.TrimSpace(cfg.APIKeyEnv); keyEnv != "" {
		if strings.ContainsAny(keyEnv, "=\x00") {
			return nil, fmt.Errorf("anthropic: invalid api_key_env %q", keyEnv)
		}
		key := os.Getenv(keyEnv)
		if key == "" {
			return nil, fmt.Errorf("anthropic: environment variable %s is not set", keyEnv)
		}
		opts = append(opts, option.WithAPIKey(key))
	}

	return &Provider{
		client:    anthropicsdk.NewClient(opts...),
		model:     model,
		effort:    effort,
		maxTokens: maxTokens,
		timeout:   timeout,
	}, nil
}

func validateBaseURL(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("anthropic: invalid base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("anthropic: base_url must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("anthropic: base_url has no host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("anthropic: base_url must not contain a query or fragment")
	}
	return nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string {
	return "anthropic:" + p.model
}

// Complete implements llm.Provider.
//
// Request.SchemaName is not used: the Anthropic format has no schema name.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("anthropic: prompt is required")
	}
	if req.MaxTokens < 0 {
		return nil, errors.New("anthropic: max_tokens must not be negative")
	}
	if req.JSONSchema != nil && len(req.JSONSchema) == 0 {
		return nil, errors.New("anthropic: json schema must not be empty")
	}

	maxTokens := int64(req.MaxTokens)
	if maxTokens == 0 {
		maxTokens = p.maxTokens
	}

	params := anthropicsdk.MessageNewParams{
		Model:     p.model,
		MaxTokens: maxTokens,
		Messages: []anthropicsdk.MessageParam{
			anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock(req.Prompt)),
		},
		Thinking: anthropicsdk.ThinkingConfigParamUnion{
			OfAdaptive: &anthropicsdk.ThinkingConfigAdaptiveParam{},
		},
		OutputConfig: anthropicsdk.OutputConfigParam{
			Effort: p.effort,
		},
	}
	if req.System != "" {
		params.System = []anthropicsdk.TextBlockParam{{Text: req.System}}
	}
	if req.JSONSchema != nil {
		params.OutputConfig.Format = anthropicsdk.JSONOutputFormatParam{Schema: req.JSONSchema}
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var (
		msg *anthropicsdk.Message
		err error
	)
	if maxTokens > streamingThreshold {
		msg, err = p.stream(ctx, params)
	} else {
		msg, err = p.client.Messages.New(ctx, params)
	}
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}

	// Check stop_reason before reading the content.
	switch msg.StopReason {
	case anthropicsdk.StopReasonEndTurn, anthropicsdk.StopReasonStopSequence:
	case anthropicsdk.StopReasonRefusal:
		return nil, fmt.Errorf("%w: category %q: %s", ErrRefused, msg.StopDetails.Category, msg.StopDetails.Explanation)
	case anthropicsdk.StopReasonMaxTokens:
		return nil, ErrTruncated
	default:
		return nil, fmt.Errorf("anthropic: unexpected stop reason %q", msg.StopReason)
	}

	var text strings.Builder
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(anthropicsdk.TextBlock); ok {
			text.WriteString(tb.Text)
		}
	}
	answer := strings.TrimSpace(text.String())
	if answer == "" {
		return nil, ErrNoAnswer
	}

	resp := &llm.Response{
		Text:  answer,
		Model: string(msg.Model),
		Usage: llm.Usage{
			// Cached input tokens are input tokens too.
			InputTokens:  int(msg.Usage.InputTokens + msg.Usage.CacheCreationInputTokens + msg.Usage.CacheReadInputTokens),
			OutputTokens: int(msg.Usage.OutputTokens),
		},
	}
	if resp.Model == "" {
		resp.Model = p.model
	}
	if req.JSONSchema != nil {
		raw, err := llm.ExtractJSONObject(answer)
		if err != nil {
			return nil, fmt.Errorf("anthropic: answer is not a JSON object: %w", err)
		}
		resp.JSON = raw
	}
	return resp, nil
}

// stream runs the request as a stream and accumulates the final message.
func (p *Provider) stream(ctx context.Context, params anthropicsdk.MessageNewParams) (*anthropicsdk.Message, error) {
	stream := p.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	var msg anthropicsdk.Message
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return nil, err
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return &msg, nil
}
