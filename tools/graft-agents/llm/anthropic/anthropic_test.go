package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// fakeAPI is an httptest stand-in for POST /v1/messages. respond receives the
// zero-based request index and the decoded request body, and returns the
// status and the raw body (JSON, or an SSE stream when the request asked for
// streaming).
type fakeAPI struct {
	t       *testing.T
	srv     *httptest.Server
	respond func(i int, body map[string]any) (int, string)

	mu     sync.Mutex
	bodies []map[string]any
	heads  []http.Header
}

func newFakeAPI(t *testing.T, respond func(int, map[string]any) (int, string)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, respond: respond}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Errorf("body is not JSON: %v", err)
	}

	f.mu.Lock()
	i := len(f.bodies)
	f.bodies = append(f.bodies, body)
	f.heads = append(f.heads, r.Header.Clone())
	f.mu.Unlock()

	status, out := f.respond(i, body)
	if body["stream"] == true {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, out)
}

func (f *fakeAPI) requests() ([]map[string]any, []http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.bodies...), append([]http.Header(nil), f.heads...)
}

// messageJSON builds a non-streaming Messages API answer.
func messageJSON(stopReason string, stopDetails any, blocks ...map[string]any) string {
	content := make([]any, 0, len(blocks))
	for _, b := range blocks {
		content = append(content, b)
	}
	msg := map[string]any{
		"id":            "msg_01",
		"type":          "message",
		"role":          "assistant",
		"model":         "claude-opus-5-5",
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"stop_details":  stopDetails,
		"usage": map[string]any{
			"input_tokens":                12,
			"cache_creation_input_tokens": 3,
			"cache_read_input_tokens":     5,
			"output_tokens":               7,
		},
	}
	b, _ := json.Marshal(msg)
	return string(b)
}

func text(s string) map[string]any {
	return map[string]any{"type": "text", "text": s, "citations": nil}
}

func okText(s string) (int, string) {
	return http.StatusOK, messageJSON("end_turn", nil, text(s))
}

// sse builds a streaming answer carrying one text block and the given stop
// reason.
func sse(parts []string, stopReason string) string {
	var b strings.Builder
	event := func(name string, data any) {
		j, _ := json.Marshal(data)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, j)
	}
	event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_02", "type": "message", "role": "assistant",
			"model": "claude-opus-5-5", "content": []any{}, "stop_reason": nil,
			"stop_sequence": nil, "stop_details": nil,
			"usage": map[string]any{"input_tokens": 20, "output_tokens": 1,
				"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
		},
	})
	event("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	for _, p := range parts {
		event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": p},
		})
	}
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil, "stop_details": nil},
		"usage": map[string]any{"output_tokens": 9},
	})
	event("message_stop", map[string]any{"type": "message_stop"})
	return b.String()
}

// newProvider returns a provider aimed at the fake, with an API key taken
// from a test-private variable.
func newProvider(t *testing.T, f *fakeAPI, mutate ...func(*llm.Config)) *Provider {
	t.Helper()
	t.Setenv("GRAFT_TEST_ANTHROPIC_KEY", "sk-ant-test-secret")

	cfg := llm.Config{
		Type:      "anthropic",
		BaseURL:   f.srv.URL,
		APIKeyEnv: "GRAFT_TEST_ANTHROPIC_KEY",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	p, err := New(llm.Config{Type: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "anthropic:claude-opus-5-5" {
		t.Fatalf("Name() = %q", p.Name())
	}
	if p.effort != anthropicsdk.OutputConfigEffortHigh {
		t.Fatalf("effort = %q, want high", p.effort)
	}
	if p.maxTokens != DefaultMaxTokens {
		t.Fatalf("maxTokens = %d", p.maxTokens)
	}
	if p.timeout != llm.DefaultTimeout {
		t.Fatalf("timeout = %v", p.timeout)
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]llm.Config{
		"unknown effort":      {Type: "anthropic", Effort: "extreme"},
		"negative max tokens": {Type: "anthropic", MaxTokens: -1},
		"bad timeout":         {Type: "anthropic", Timeout: "later"},
		"ftp base url":        {Type: "anthropic", BaseURL: "ftp://api.example.com"},
		"base url no host":    {Type: "anthropic", BaseURL: "https://"},
		"base url query":      {Type: "anthropic", BaseURL: "https://api.example.com?x=1"},
		"key env with equals": {Type: "anthropic", APIKeyEnv: "A=B"},
		"unset key env":       {Type: "anthropic", APIKeyEnv: "GRAFT_TEST_DEFINITELY_UNSET_VAR"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestEffortCaseInsensitiveAndAccepted(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]anthropicsdk.OutputConfigEffort{
		"LOW":    anthropicsdk.OutputConfigEffortLow,
		" max ":  anthropicsdk.OutputConfigEffortMax,
		"xhigh":  anthropicsdk.OutputConfigEffortXhigh,
		"medium": anthropicsdk.OutputConfigEffortMedium,
	} {
		p, err := New(llm.Config{Type: "anthropic", Effort: in})
		if err != nil {
			t.Fatalf("effort %q: %v", in, err)
		}
		if p.effort != want {
			t.Errorf("effort %q = %q, want %q", in, p.effort, want)
		}
	}
}

func TestRegisteredInRegistry(t *testing.T) {
	t.Parallel()

	p, err := llm.New(llm.Config{Type: "anthropic", Model: "claude-sonnet-5-5"})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	if p.Name() != "anthropic:claude-sonnet-5-5" {
		t.Fatalf("Name() = %q", p.Name())
	}
}

func TestCompleteRequestShape(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) { return okText("merged") })
	p := newProvider(t, f, func(c *llm.Config) { c.Effort = "max" })

	resp, err := p.Complete(context.Background(), llm.Request{System: "be exact", Prompt: "merge these"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "merged" || resp.JSON != nil {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Model != "claude-opus-5-5" {
		t.Fatalf("Model = %q", resp.Model)
	}
	if resp.Usage.InputTokens != 20 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("Usage = %+v (cache tokens must count as input)", resp.Usage)
	}

	bodies, heads := f.requests()
	if len(bodies) != 1 {
		t.Fatalf("requests = %d", len(bodies))
	}
	body := bodies[0]
	if body["model"] != "claude-opus-5-5" || body["max_tokens"] != float64(DefaultMaxTokens) {
		t.Fatalf("model/max_tokens = %v / %v", body["model"], body["max_tokens"])
	}
	if body["stream"] == true {
		t.Fatal("streamed although max_tokens is below the threshold")
	}
	if th := body["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Fatalf("thinking = %v", th)
	}
	oc := body["output_config"].(map[string]any)
	if oc["effort"] != "max" {
		t.Fatalf("output_config.effort = %v", oc["effort"])
	}
	if _, ok := oc["format"]; ok {
		t.Fatal("format sent without a schema")
	}
	sys := body["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["text"] != "be exact" {
		t.Fatalf("system = %v", sys)
	}
	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"].([]any)[0].(map[string]any)["text"] != "merge these" {
		t.Fatalf("messages = %v", msgs)
	}

	if key := heads[0].Get("X-Api-Key"); key != "sk-ant-test-secret" {
		t.Fatalf("x-api-key = %q", key)
	}
}

func TestCompleteOmitsSystemWhenEmpty(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) { return okText("ok") })
	p := newProvider(t, f)

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	bodies, _ := f.requests()
	if _, ok := bodies[0]["system"]; ok {
		t.Fatalf("empty system sent: %v", bodies[0]["system"])
	}
}

func TestRequestMaxTokensOverridesDefault(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) { return okText("ok") })
	p := newProvider(t, f, func(c *llm.Config) { c.MaxTokens = 2048 })

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi", MaxTokens: 77}); err != nil {
		t.Fatal(err)
	}
	bodies, _ := f.requests()
	if bodies[0]["max_tokens"] != float64(2048) || bodies[1]["max_tokens"] != float64(77) {
		t.Fatalf("max_tokens = %v, %v", bodies[0]["max_tokens"], bodies[1]["max_tokens"])
	}
}

func TestCompleteWithSchemaSendsFormatAndParsesJSON(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, messageJSON("end_turn", nil, text(`{"content":"x","confidence":"high"}`))
	})
	p := newProvider(t, f)
	schema := map[string]any{"type": "object", "properties": map[string]any{"content": map[string]any{"type": "string"}}}

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "resolve", JSONSchema: schema})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if string(resp.JSON) != `{"content":"x","confidence":"high"}` {
		t.Fatalf("JSON = %s", resp.JSON)
	}

	bodies, _ := f.requests()
	format := bodies[0]["output_config"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("format.type = %v", format["type"])
	}
	if format["schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("schema not forwarded: %v", format["schema"])
	}
}

func TestSchemaAnswerNotJSON(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) { return okText("I cannot do that.") })
	p := newProvider(t, f)

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x", JSONSchema: map[string]any{"type": "object"}}); err == nil {
		t.Fatal("expected error")
	}
}

func TestOnlyTextBlocksAreAnswer(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, messageJSON("end_turn", nil,
			map[string]any{"type": "thinking", "thinking": "secret reasoning", "signature": "sig"},
			text("Hello, "),
			text("world."),
		)
	})
	p := newProvider(t, f)

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "Hello, world." {
		t.Fatalf("Text = %q", resp.Text)
	}
}

func TestRefusalIsAnError(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, messageJSON("refusal",
			map[string]any{"type": "refusal", "category": "cyber", "explanation": "not doing that"})
	})
	p := newProvider(t, f)

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if resp != nil {
		t.Fatalf("partial answer returned on refusal: %+v", resp)
	}
	if !strings.Contains(err.Error(), "cyber") || !strings.Contains(err.Error(), "not doing that") {
		t.Fatalf("error lacks category or explanation: %v", err)
	}
}

func TestRefusalWithTextIsNotReturned(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, messageJSON("refusal",
			map[string]any{"type": "refusal", "category": nil, "explanation": "no"}, text("partial text"))
	})
	p := newProvider(t, f)

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

func TestMaxTokensIsTruncationError(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, messageJSON("max_tokens", nil, text("half an answer"))
	})
	p := newProvider(t, f)

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x", JSONSchema: map[string]any{"type": "object"}}); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestUnexpectedStopReason(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, messageJSON("tool_use", nil, text("calling a tool"))
	})
	p := newProvider(t, f)

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "tool_use") {
		t.Fatalf("err = %v", err)
	}
}

func TestStopSequenceIsAccepted(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, messageJSON("stop_sequence", nil, text("ok"))
	})
	p := newProvider(t, f)

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	if err != nil || resp.Text != "ok" {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
}

func TestNoTextIsEmptyAnswer(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, messageJSON("end_turn", nil,
			map[string]any{"type": "thinking", "thinking": "", "signature": "sig"})
	})
	p := newProvider(t, f)

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x"}); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err = %v, want ErrNoAnswer", err)
	}
}

func TestAPIErrorIsReturned(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		return http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens too large"}}`
	})
	p := newProvider(t, f)

	_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	var apiErr *anthropicsdk.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want SDK API error 400", err)
	}
	if strings.Contains(err.Error(), "sk-ant-test-secret") {
		t.Fatal("error leaks the API key")
	}
	if n := len(must(f.requests())); n != 1 {
		t.Fatalf("400 retried: %d requests", n)
	}
}

// must returns the first value of a two-value call, for brevity in asserts.
func must[T any, U any](a T, _ U) T { return a }

func TestLongAnswerIsStreamed(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, sse([]string{`{"content": `, `"merged"}`}, "end_turn")
	})
	p := newProvider(t, f)

	resp, err := p.Complete(context.Background(), llm.Request{
		Prompt:     "big",
		MaxTokens:  32000,
		JSONSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if string(resp.JSON) != `{"content": "merged"}` {
		t.Fatalf("JSON = %s", resp.JSON)
	}
	if resp.Usage.OutputTokens != 9 || resp.Usage.InputTokens != 20 {
		t.Fatalf("Usage = %+v (streamed usage not accumulated)", resp.Usage)
	}
	bodies, _ := f.requests()
	if bodies[0]["stream"] != true || bodies[0]["max_tokens"] != float64(32000) {
		t.Fatalf("stream/max_tokens = %v / %v", bodies[0]["stream"], bodies[0]["max_tokens"])
	}
}

func TestStreamedRefusalAndTruncation(t *testing.T) {
	for stop, want := range map[string]error{"max_tokens": ErrTruncated, "refusal": ErrRefused} {
		stop, want := stop, want
		f := newFakeAPI(t, func(int, map[string]any) (int, string) {
			return http.StatusOK, sse([]string{"partial"}, stop)
		})
		p := newProvider(t, f)

		_, err := p.Complete(context.Background(), llm.Request{Prompt: "x", MaxTokens: 20000})
		if !errors.Is(err, want) {
			t.Errorf("stop %s: err = %v, want %v", stop, err, want)
		}
	}
}

func TestStreamErrorEventFails(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"broken\"}}\n\n"
	})
	p := newProvider(t, f)

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x", MaxTokens: 20000}); err == nil {
		t.Fatal("expected error from an error event")
	}
}

func TestTimeoutFromConfig(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) {
		time.Sleep(2 * time.Second)
		return okText("late")
	})
	p := newProvider(t, f, func(c *llm.Config) { c.Timeout = "150ms" })

	start := time.Now()
	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x"}); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("timeout not enforced, took %v", time.Since(start))
	}
}

func TestCancelledContext(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) { return okText("ok") })
	p := newProvider(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Complete(ctx, llm.Request{Prompt: "x"}); err == nil {
		t.Fatal("expected error for a cancelled context")
	}
}

func TestValidationBeforeAnyRequest(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) { return okText("unused") })
	p := newProvider(t, f)

	for name, req := range map[string]llm.Request{
		"empty prompt": {Prompt: "\n\t"},
		"negative max": {Prompt: "x", MaxTokens: -3},
		"empty schema": {Prompt: "x", JSONSchema: map[string]any{}},
	} {
		if _, err := p.Complete(context.Background(), req); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if n := len(must(f.requests())); n != 0 {
		t.Fatalf("invalid requests reached the API: %d", n)
	}
}

func TestDefaultAPIKeyResolution(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) { return okText("ok") })
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-from-sdk-default")
	p, err := New(llm.Config{Type: "anthropic", BaseURL: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	_, heads := f.requests()
	if key := heads[0].Get("X-Api-Key"); key != "sk-ant-from-sdk-default" {
		t.Fatalf("x-api-key = %q, want the SDK default variable", key)
	}
}

func TestConcurrentCompletes(t *testing.T) {
	f := newFakeAPI(t, func(int, map[string]any) (int, string) { return okText("ok") })
	p := newProvider(t, f)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
			if err == nil && resp.Text != "ok" {
				err = fmt.Errorf("Text = %q", resp.Text)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := len(must(f.requests())); n != 8 {
		t.Fatalf("requests = %d, want 8", n)
	}
}

func TestProviderSatisfiesInterface(t *testing.T) {
	t.Parallel()

	var _ llm.Provider = (*Provider)(nil)
}
