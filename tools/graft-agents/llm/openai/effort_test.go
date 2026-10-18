package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

func TestEffortSentAsReasoning(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("ok")))
	p := newProvider(t, f, func(c *llm.Config) { c.Effort = " Low " })

	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	r, ok := f.requests()[0].body["reasoning"].(map[string]any)
	if !ok || r["effort"] != "low" || len(r) != 1 {
		t.Fatalf("reasoning = %v, want {\"effort\": \"low\"}", f.requests()[0].body["reasoning"])
	}
}

func TestEffortKeptOnSchemaFallback(t *testing.T) {
	// The effort is part of every request, whatever response format the call falls back to.
	f := newFakeServer(t, scripted(
		reply{status: 400, body: `{"error":{"message":"response_format json_schema is not supported"}}`},
		chatOK(`{"a":1}`),
	))
	p := newProvider(t, f, func(c *llm.Config) { c.Effort = "high" })
	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi", JSONSchema: map[string]any{"type": "object"}}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests()); n != 2 {
		t.Fatalf("%d requests, want the json_schema one and its fallback", n)
	}
	for i, rec := range f.requests() {
		if r, _ := rec.body["reasoning"].(map[string]any); r["effort"] != "high" {
			t.Fatalf("request %d: reasoning = %v", i, rec.body["reasoning"])
		}
	}
}

func TestNoEffortSendsNoReasoning(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("ok")))
	p := newProvider(t, f)
	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.requests()[0].body["reasoning"]; ok {
		t.Fatalf("reasoning sent without an effort: %v", f.requests()[0].body)
	}
}

func TestNewRejectsUnsupportedSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  llm.Config
		want string
	}{
		"unknown effort": {llm.Config{Type: "openai", Model: "m", Effort: "turbo"}, `invalid effort "turbo"`},
		"options":        {llm.Config{Type: "openai", Model: "m", Options: map[string]string{"region": "eu", "a": "b"}}, "unsupported options [a region]"},
	} {
		_, err := New(tc.cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	for _, e := range []string{"minimal", "low", "medium", "high", "xhigh"} {
		if _, err := New(llm.Config{Type: "openai", Model: "m", Effort: e}); err != nil {
			t.Errorf("effort %s: %v", e, err)
		}
	}
}
