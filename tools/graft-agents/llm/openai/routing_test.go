package openai

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

func TestRoutingSentAsProvider(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("ok")))
	p := newProvider(t, f, func(c *llm.Config) {
		c.Routing = &llm.Routing{Only: []string{"openai", "azure/eu"}, Ignore: []string{"openai/ultrafast"}}
	})
	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"only":            []any{"openai", "azure/eu"},
		"ignore":          []any{"openai/ultrafast"},
		"allow_fallbacks": false,
	}
	if got := f.requests()[0].body["provider"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("provider = %v, want %v", got, want)
	}
}

func TestNoRoutingSendsNoProvider(t *testing.T) {
	f := newFakeServer(t, scripted(chatOK("ok")))
	p := newProvider(t, f)
	if _, err := p.Complete(context.Background(), llm.Request{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.requests()[0].body["provider"]; ok {
		t.Fatalf("provider sent without routing: %v", f.requests()[0].body)
	}
}

func TestNewRejectsInvalidRouting(t *testing.T) {
	for name, tc := range map[string]struct {
		routing llm.Routing
		want    string
	}{
		"empty":       {llm.Routing{}, "names no provider"},
		"bad name":    {llm.Routing{Only: []string{"Open AI"}}, `"Open AI" is not a provider slug`},
		"bad variant": {llm.Routing{Ignore: []string{"openai/"}}, `"openai/" is not a provider slug`},
		"both":        {llm.Routing{Only: []string{"openai"}, Ignore: []string{"openai"}}, "listed in both only and ignore"},
		"twice":       {llm.Routing{Only: []string{"azure", "azure"}}, "listed twice"},
	} {
		r := tc.routing
		_, err := New(llm.Config{Type: "openai", Model: "m", Routing: &r})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
