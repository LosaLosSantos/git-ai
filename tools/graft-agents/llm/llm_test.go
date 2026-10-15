package llm

import (
	"context"
	"errors"
	"testing"
)

type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake:model" }

func (fakeProvider) Complete(context.Context, Request) (*Response, error) {
	return &Response{Text: "ok"}, nil
}

func TestRegistry(t *testing.T) {
	Register("test-fake", func(Config) (Provider, error) { return fakeProvider{}, nil })

	p, err := New(Config{Type: "test-fake"})
	if err != nil || p.Name() != "fake:model" {
		t.Fatalf("New: %v %v", p, err)
	}
	if _, err := New(Config{Type: "none"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("none: %v", err)
	}
	if _, err := New(Config{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := New(Config{Type: "nope"}); err == nil {
		t.Fatal("unknown type accepted")
	}
}

func TestExtractJSONObject(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`: `{"a":1}`,
		"Here you go:\n```json\n{\"a\": {\"b\": 2}}\n```": `{"a": {"b": 2}}`,
		`prose {not json} then {"x":"}"} end`:             `{"x":"}"}`,
	} {
		got, err := ExtractJSONObject(in)
		if err != nil || string(got) != want {
			t.Errorf("ExtractJSONObject(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := ExtractJSONObject("no json here"); err == nil {
		t.Error("expected error")
	}
}

func TestTimeout(t *testing.T) {
	if d, err := (Config{}).TimeoutOrDefault(); err != nil || d != DefaultTimeout {
		t.Fatal(d, err)
	}
	if _, err := (Config{Timeout: "x"}).TimeoutOrDefault(); err == nil {
		t.Fatal("bad timeout accepted")
	}
}
