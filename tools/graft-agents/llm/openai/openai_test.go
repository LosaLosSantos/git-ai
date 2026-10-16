package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// reply is one scripted answer of the fake endpoint.
type reply struct {
	status int
	header http.Header
	body   string
}

// recorded is one request seen by the fake endpoint.
type recorded struct {
	path   string
	auth   string
	body   map[string]any
	system string
	format string // response_format.type, empty when absent
}

// fakeServer answers requests with respond, which receives the zero-based
// request index and the parsed request, and records every request.
type fakeServer struct {
	t       *testing.T
	srv     *httptest.Server
	respond func(i int, rec recorded) reply

	mu   sync.Mutex
	reqs []recorded
}

func newFakeServer(t *testing.T, respond func(i int, rec recorded) reply) *fakeServer {
	t.Helper()
	f := &fakeServer{t: t, respond: respond}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// scripted returns the replies in order; the last one repeats.
func scripted(replies ...reply) func(int, recorded) reply {
	return func(i int, _ recorded) reply {
		if i >= len(replies) {
			return replies[len(replies)-1]
		}
		return replies[i]
	}
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read request: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Errorf("request body is not JSON: %v", err)
	}

	rec := recorded{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			msg := m.(map[string]any)
			if msg["role"] == "system" {
				rec.system, _ = msg["content"].(string)
			}
		}
	}
	if rf, ok := body["response_format"].(map[string]any); ok {
		rec.format, _ = rf["type"].(string)
	}

	f.mu.Lock()
	i := len(f.reqs)
	f.reqs = append(f.reqs, rec)
	f.mu.Unlock()

	rep := f.respond(i, rec)
	for k, vs := range rep.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rep.status)
	_, _ = io.WriteString(w, rep.body)
}

func (f *fakeServer) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func chatOK(content string) reply {
	return reply{status: http.StatusOK, body: chatBody(content, "stop")}
}

func chatBody(content, finish string) string {
	b, _ := json.Marshal(map[string]any{
		"model": "served-model",
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": finish,
		}},
		"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 7},
	})
	return string(b)
}

// newProvider returns a provider pointed at the fake, with no backoff delay
// and an API key from a test-private variable.
func newProvider(t *testing.T, f *fakeServer, mutate ...func(*llm.Config)) *Provider {
	t.Helper()
	t.Setenv("GRAFT_TEST_OPENAI_KEY", "sk-test-secret")

	cfg := llm.Config{
		Type:      "openai",
		Model:     "test-model",
		BaseURL:   f.srv.URL + "/v1",
		APIKeyEnv: "GRAFT_TEST_OPENAI_KEY",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.client = f.srv.Client()
	p.backoff = func(int) time.Duration { return 0 }
	return p
}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]llm.Config{
		"no model":            {Type: "openai", BaseURL: "http://localhost:1/v1"},
		"ftp base url":        {Type: "openai", Model: "m", BaseURL: "ftp://example.com/v1"},
		"no host":             {Type: "openai", Model: "m", BaseURL: "http:///v1"},
		"query in base url":   {Type: "openai", Model: "m", BaseURL: "http://localhost/v1?x=1"},
		"unparsable base url": {Type: "openai", Model: "m", BaseURL: "http://[::1"},
		"negative max":        {Type: "openai", Model: "m", MaxTokens: -1},
		"bad timeout":         {Type: "openai", Model: "m", Timeout: "later"},
		"key env with equals": {Type: "openai", Model: "m", APIKeyEnv: "A=B"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDefaultsAndName(t *testing.T) {
	t.Parallel()

	p, err := New(llm.Config{Type: "openai", Model: " gpt-4o-mini "})
	if err != nil {
		t.Fatal(err)
	}
	if p.endpoint != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("endpoint = %q", p.endpoint)
	}
	if p.Name() != "openai:gpt-4o-mini" {
		t.Fatalf("Name() = %q", p.Name())
	}
	if p.timeout != llm.DefaultTimeout {
		t.Fatalf("timeout = %v", p.timeout)
	}
}

func TestBaseURLTrailingSlash(t *testing.T) {
	t.Parallel()

	p, err := New(llm.Config{Type: "openai", Model: "m", BaseURL: "http://localhost:11434/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	if p.endpoint != "http://localhost:11434/v1/chat/completions" {
		t.Fatalf("endpoint = %q", p.endpoint)
	}
}

func TestRegisteredInRegistry(t *testing.T) {
	t.Parallel()

	p, err := llm.New(llm.Config{Type: "openai", Model: "llama3.1", BaseURL: "http://localhost:11434/v1"})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	if p.Name() != "openai:llama3.1" {
		t.Fatalf("Name() = %q", p.Name())
	}
}

func TestCompleteTextRequestShape(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("merged text")))
	p := newProvider(t, f, func(c *llm.Config) { c.MaxTokens = 900 })

	resp, err := p.Complete(context.Background(), llm.Request{System: "be terse", Prompt: "merge"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "merged text" || resp.JSON != nil {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Model != "served-model" {
		t.Fatalf("Model = %q, want the served model", resp.Model)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("Usage = %+v", resp.Usage)
	}

	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	got := reqs[0]
	if got.path != "/v1/chat/completions" {
		t.Fatalf("path = %q", got.path)
	}
	if got.auth != "Bearer sk-test-secret" {
		t.Fatalf("Authorization = %q", got.auth)
	}
	if got.body["model"] != "test-model" || got.body["max_tokens"] != float64(900) {
		t.Fatalf("body = %v", got.body)
	}
	if _, ok := got.body["response_format"]; ok {
		t.Fatal("response_format sent without a schema")
	}
	if got.system != "be terse" {
		t.Fatalf("system = %q", got.system)
	}
	msgs := got.body["messages"].([]any)
	if len(msgs) != 2 || msgs[1].(map[string]any)["content"] != "merge" {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestCompleteOmitsEmptySystemAndZeroMaxTokens(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("ok")))
	p := newProvider(t, f)

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	got := f.requests()[0]
	if _, ok := got.body["max_tokens"]; ok {
		t.Fatalf("max_tokens sent when unset: %v", got.body)
	}
	msgs := got.body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestRequestMaxTokensOverridesDefault(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("ok")))
	p := newProvider(t, f, func(c *llm.Config) { c.MaxTokens = 900 })

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi", MaxTokens: 50}); err != nil {
		t.Fatal(err)
	}
	if got := f.requests()[0].body["max_tokens"]; got != float64(50) {
		t.Fatalf("max_tokens = %v, want 50", got)
	}
}

func TestAPIKeyOptionalForLocalServers(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("ok")))
	t.Setenv("GRAFT_TEST_OPENAI_KEY", "")
	p, err := New(llm.Config{Type: "openai", Model: "m", BaseURL: f.srv.URL + "/v1", APIKeyEnv: "GRAFT_TEST_OPENAI_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	p.client = f.srv.Client()

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if auth := f.requests()[0].auth; auth != "" {
		t.Fatalf("Authorization sent without a key: %q", auth)
	}
}

func TestDefaultAPIKeyEnvIsOpenAIKey(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("ok")))
	t.Setenv(DefaultAPIKeyEnv, "sk-default")
	p, err := New(llm.Config{Type: "openai", Model: "m", BaseURL: f.srv.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	p.client = f.srv.Client()

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if auth := f.requests()[0].auth; auth != "Bearer sk-default" {
		t.Fatalf("Authorization = %q", auth)
	}
}

func TestSchemaFirstAttemptUsesJSONSchema(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK(`{"content":"x","confidence":"high"}`)))
	p := newProvider(t, f)
	schema := map[string]any{"type": "object", "required": []any{"content"}}

	resp, err := p.Complete(context.Background(), llm.Request{
		Prompt:     "resolve",
		JSONSchema: schema,
		SchemaName: "resolution",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if string(resp.JSON) != `{"content":"x","confidence":"high"}` {
		t.Fatalf("JSON = %s", resp.JSON)
	}

	got := f.requests()[0]
	if got.format != "json_schema" {
		t.Fatalf("response_format.type = %q", got.format)
	}
	rf := got.body["response_format"].(map[string]any)["json_schema"].(map[string]any)
	if rf["name"] != "resolution" || rf["strict"] != true {
		t.Fatalf("json_schema = %v", rf)
	}
	if rf["schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("schema not forwarded: %v", rf["schema"])
	}
	if strings.Contains(got.system, "JSON Schema") {
		t.Fatal("schema instruction added although the server supports json_schema")
	}
}

func TestSchemaDefaultNameAndExtraction(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("```json\n{\"ok\": true}\n```")))
	p := newProvider(t, f)

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x", JSONSchema: map[string]any{"type": "object"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.JSON) != `{"ok": true}` {
		t.Fatalf("JSON = %s", resp.JSON)
	}
	name := f.requests()[0].body["response_format"].(map[string]any)["json_schema"].(map[string]any)["name"]
	if name != "graft_answer" {
		t.Fatalf("default schema name = %v", name)
	}
}

func TestSchemaFallsBackToJSONObjectAndRemembers(t *testing.T) {
	f := newFakeServer(t, func(i int, rec recorded) reply {
		if rec.format == "json_schema" {
			return reply{status: http.StatusBadRequest, body: `{"error":"json_schema unsupported"}`}
		}
		return chatOK(`{"ok":true}`)
	})
	p := newProvider(t, f)
	schema := map[string]any{"type": "object"}

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x", JSONSchema: schema, System: "sys"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if string(resp.JSON) != `{"ok":true}` {
		t.Fatalf("JSON = %s", resp.JSON)
	}

	reqs := f.requests()
	if len(reqs) != 2 || reqs[0].format != "json_schema" || reqs[1].format != "json_object" {
		t.Fatalf("formats = %+v", reqs)
	}
	if !strings.HasPrefix(reqs[1].system, "sys\n\n") || !strings.Contains(reqs[1].system, `"type":"object"`) {
		t.Fatalf("json_object request lacks the schema instruction: %q", reqs[1].system)
	}
	if p.level.Load() != levelJSONObject {
		t.Fatalf("level = %d, want json_object", p.level.Load())
	}

	// The next call starts at the remembered level: no json_schema attempt.
	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "y", JSONSchema: schema}); err != nil {
		t.Fatal(err)
	}
	reqs = f.requests()
	if len(reqs) != 3 || reqs[2].format != "json_object" {
		t.Fatalf("second call formats wrong: %+v", reqs)
	}
}

func TestSchemaFallsBackToPlainTextWithExtraction(t *testing.T) {
	f := newFakeServer(t, func(i int, rec recorded) reply {
		if rec.format != "" {
			return reply{status: http.StatusBadRequest, body: `{"error":"no response_format"}`}
		}
		return chatOK("Sure! {\"ok\": true} Hope it helps.")
	})
	p := newProvider(t, f)

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x", JSONSchema: map[string]any{"type": "object"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if string(resp.JSON) != `{"ok": true}` {
		t.Fatalf("JSON = %s", resp.JSON)
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	last := reqs[2]
	if last.format != "" {
		t.Fatalf("plain-text attempt sent response_format %q", last.format)
	}
	if !strings.Contains(last.system, "JSON Schema") {
		t.Fatal("plain-text attempt lacks the schema instruction")
	}
	if p.level.Load() != levelText {
		t.Fatalf("level = %d, want text", p.level.Load())
	}
}

func TestSchemaAllFormatsRejected(t *testing.T) {
	f := newFakeServer(t, scripted(reply{status: http.StatusBadRequest, body: `{"error":"bad request"}`}))
	p := newProvider(t, f)

	_, err := p.Complete(context.Background(), llm.Request{Prompt: "x", JSONSchema: map[string]any{"type": "object"}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want APIError 400", err)
	}
	if n := len(f.requests()); n != 3 {
		t.Fatalf("requests = %d, want 3 (one per format)", n)
	}
	if p.level.Load() != levelJSONSchema {
		t.Fatalf("level moved although no weaker format was accepted: %d", p.level.Load())
	}
}

func TestUnrelatedBadRequestKeepsSchemaFormat(t *testing.T) {
	// A 400 that has nothing to do with response_format (here an oversized
	// prompt) fails with every format. It must not downgrade later calls.
	f := newFakeServer(t, func(_ int, rec recorded) reply {
		msgs := rec.body["messages"].([]any)
		user := msgs[len(msgs)-1].(map[string]any)["content"].(string)
		if strings.Contains(user, "HUGE") {
			return reply{status: http.StatusBadRequest, body: `{"error":{"code":"context_length_exceeded"}}`}
		}
		return chatOK(`{"a":"b"}`)
	})
	p := newProvider(t, f)
	schema := map[string]any{"type": "object"}

	_, err := p.Complete(context.Background(), llm.Request{Prompt: "HUGE prompt", JSONSchema: schema})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want APIError 400", err)
	}
	if p.level.Load() != levelJSONSchema {
		t.Fatalf("level = %d after an unrelated 400, want json_schema", p.level.Load())
	}

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "small prompt", JSONSchema: schema})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if string(resp.JSON) != `{"a":"b"}` {
		t.Fatalf("JSON = %s", resp.JSON)
	}
	reqs := f.requests()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want 4", len(reqs))
	}
	if reqs[3].format != "json_schema" {
		t.Fatalf("later call sent response_format %q, want json_schema", reqs[3].format)
	}
}

func TestPlainTextRequestDoesNotFallBack(t *testing.T) {
	f := newFakeServer(t, scripted(reply{status: http.StatusBadRequest, body: `{"error":"model missing"}`}))
	p := newProvider(t, f)

	_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.requests()); n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
	if p.level.Load() != levelJSONSchema {
		t.Fatalf("level moved on a plain-text 400: %d", p.level.Load())
	}
}

func TestRetriesOn429And5xx(t *testing.T) {
	f := newFakeServer(t, scripted(
		reply{status: http.StatusTooManyRequests, body: `{}`},
		reply{status: http.StatusServiceUnavailable, body: `{}`},
		chatOK("done"),
	))
	p := newProvider(t, f)
	var waits []int
	p.backoff = func(attempt int) time.Duration {
		waits = append(waits, attempt)
		return 0
	}

	resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "done" {
		t.Fatalf("Text = %q", resp.Text)
	}
	if n := len(f.requests()); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
	if len(waits) != 2 || waits[0] != 0 || waits[1] != 1 {
		t.Fatalf("backoff attempts = %v", waits)
	}
}

func TestRetriesExhausted(t *testing.T) {
	f := newFakeServer(t, scripted(reply{status: http.StatusInternalServerError, body: "boom"}))
	p := newProvider(t, f)

	_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.requests()); n != 1+MaxRetries {
		t.Fatalf("requests = %d, want %d", n, 1+MaxRetries)
	}
}

func TestNoRetryOnClientError(t *testing.T) {
	f := newFakeServer(t, scripted(reply{status: http.StatusUnauthorized, body: `{"error":"bad key"}`}))
	p := newProvider(t, f)

	_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.requests()); n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
}

func TestAPIErrorNeverEchoesKey(t *testing.T) {
	f := newFakeServer(t, scripted(reply{status: http.StatusUnauthorized, body: "invalid key"}))
	p := newProvider(t, f)

	_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	if err == nil || strings.Contains(err.Error(), "sk-test-secret") {
		t.Fatalf("error leaks the key or is nil: %v", err)
	}
}

func TestAPIErrorBodyIsBounded(t *testing.T) {
	f := newFakeServer(t, scripted(reply{status: http.StatusBadRequest, body: strings.Repeat("é", 4000)}))
	p := newProvider(t, f)

	_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	if len(apiErr.Message) > maxErrorBodyBytes {
		t.Fatalf("message length %d exceeds bound", len(apiErr.Message))
	}
}

func TestRetryAfterHeaderParsing(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]time.Duration{
		"":      0,
		"2":     2 * time.Second,
		" 3 ":   3 * time.Second,
		"0":     0,
		"-4":    0,
		"soon":  0,
		"99999": maxRetryAfter,
	} {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestUnusableRetryAfterFallsBackToBackoff(t *testing.T) {
	f := newFakeServer(t, scripted(
		reply{status: http.StatusTooManyRequests, header: http.Header{"Retry-After": []string{"0"}}, body: `{}`},
		chatOK("ok"),
	))
	p := newProvider(t, f)
	called := false
	p.backoff = func(int) time.Duration {
		called = true
		return 0
	}

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("backoff not used although Retry-After was unusable (0)")
	}
}

func TestDefaultBackoffGrowsAndCaps(t *testing.T) {
	t.Parallel()

	if defaultBackoff(0) != baseBackoff || defaultBackoff(1) != 2*baseBackoff {
		t.Fatalf("backoff not exponential: %v %v", defaultBackoff(0), defaultBackoff(1))
	}
	if defaultBackoff(30) != maxBackoff {
		t.Fatalf("backoff not capped: %v", defaultBackoff(30))
	}
}

func TestCancellationDuringBackoff(t *testing.T) {
	f := newFakeServer(t, scripted(reply{status: http.StatusServiceUnavailable, body: `{}`}))
	p := newProvider(t, f)
	p.backoff = func(int) time.Duration { return time.Hour }

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.Complete(ctx, llm.Request{Prompt: "x"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("backoff ignored the context")
	}
}

func TestTimeoutFromConfig(t *testing.T) {
	f := newFakeServer(t, func(int, recorded) reply {
		time.Sleep(2 * time.Second)
		return chatOK("late")
	})
	p := newProvider(t, f, func(c *llm.Config) { c.Timeout = "100ms" })

	_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestRefusals(t *testing.T) {
	refusal := `{"model":"m","choices":[{"message":{"role":"assistant","content":null,"refusal":"I can't help with that."},"finish_reason":"stop"}]}`
	filtered := chatBody("", "content_filter")

	for name, body := range map[string]string{"refusal field": refusal, "content filter": filtered} {
		f := newFakeServer(t, scripted(reply{status: http.StatusOK, body: body}))
		p := newProvider(t, f)
		if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x"}); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: err = %v, want ErrRefused", name, err)
		}
	}
}

func TestTruncatedAnswer(t *testing.T) {
	f := newFakeServer(t, scripted(reply{status: http.StatusOK, body: chatBody("partial", "length")}))
	p := newProvider(t, f)

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x", JSONSchema: map[string]any{"type": "object"}}); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestEmptyAnswers(t *testing.T) {
	for name, body := range map[string]string{
		"no choices":   `{"choices":[]}`,
		"null content": `{"choices":[{"message":{"content":null},"finish_reason":"stop"}]}`,
		"blank":        chatBody("   \n", "stop"),
	} {
		f := newFakeServer(t, scripted(reply{status: http.StatusOK, body: body}))
		p := newProvider(t, f)
		_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
		if err == nil {
			t.Errorf("%s: expected error", name)
		}
		if name == "blank" && !errors.Is(err, ErrNoAnswer) {
			t.Errorf("blank: err = %v, want ErrNoAnswer", err)
		}
	}
}

func TestSchemaAnswerNotJSON(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("I could not resolve this.")))
	p := newProvider(t, f)

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x", JSONSchema: map[string]any{"type": "object"}}); err == nil {
		t.Fatal("expected error for a non-JSON schema answer")
	}
}

func TestInvalidResponseJSON(t *testing.T) {
	f := newFakeServer(t, scripted(reply{status: http.StatusOK, body: `<html>proxy</html>`}))
	p := newProvider(t, f)

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "invalid response JSON") {
		t.Fatalf("err = %v", err)
	}
}

func TestResponseSizeBounded(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK(strings.Repeat("a", 4096))))
	p := newProvider(t, f)
	p.maxResponseBytes = 1024

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want size error", err)
	}
}

func TestValidationBeforeAnyRequest(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("unused")))
	p := newProvider(t, f)

	for name, req := range map[string]llm.Request{
		"empty prompt":    {Prompt: " "},
		"negative max":    {Prompt: "x", MaxTokens: -1},
		"empty schema":    {Prompt: "x", JSONSchema: map[string]any{}},
		"bad schema name": {Prompt: "x", JSONSchema: map[string]any{"type": "object"}, SchemaName: "bad name!"},
		"too long name":   {Prompt: "x", JSONSchema: map[string]any{"type": "object"}, SchemaName: strings.Repeat("a", 65)},
	} {
		if _, err := p.Complete(context.Background(), req); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("invalid requests reached the endpoint: %d", n)
	}
}

func TestConcurrentCallsShareFallbackLevel(t *testing.T) {
	f := newFakeServer(t, func(_ int, rec recorded) reply {
		if rec.format == "json_schema" {
			return reply{status: http.StatusBadRequest, body: `{}`}
		}
		return chatOK(`{"ok":true}`)
	})
	p := newProvider(t, f)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := p.Complete(context.Background(), llm.Request{Prompt: "x", JSONSchema: map[string]any{"type": "object"}})
			if err == nil && string(resp.JSON) != `{"ok":true}` {
				err = errors.New("wrong JSON " + string(resp.JSON))
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
	if p.level.Load() != levelJSONObject {
		t.Fatalf("level = %d, want json_object", p.level.Load())
	}
}

func TestProviderSatisfiesInterface(t *testing.T) {
	t.Parallel()

	var _ llm.Provider = (*Provider)(nil)
}
