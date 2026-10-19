package bench

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

func TestOpenRouterPricing(t *testing.T) {
	bodies := map[string]string{
		"/models/a/ok/endpoints": `{"data":{"endpoints":[
			{"name":"x","pricing":{"prompt":"0.000001","completion":"0.000004"}},
			{"name":"y","pricing":{"prompt":"0.0000015","completion":"0.000002","request":"0.0001","internal_reasoning":"0.000003"}}]}}`,
		"/models/a/none/endpoints":     `{"data":{"endpoints":[]}}`,
		"/models/a/nocomp/endpoints":   `{"data":{"endpoints":[{"name":"x","pricing":{"prompt":"0.000001"}}]}}`,
		"/models/a/variable/endpoints": `{"data":{"endpoints":[{"name":"x","pricing":{"prompt":"-1","completion":"-1"}}]}}`,
		"/models/a/free/endpoints":     `{"data":{"endpoints":[{"name":"x","pricing":{"prompt":"0","completion":"0"}}]}}`,
		"/models/a/junk/endpoints":     `<html>`,
		// The shape OpenRouter serves (2026-10): a numeric discount, prices of features the run
		// does not use, and override tiers for long prompts.
		"/models/a/real/endpoints": `{"data":{"id":"a/real","endpoints":[
			{"name":"p1","pricing":{"prompt":"0.0000001","completion":"0.0000005","web_search":"0.01","input_cache_read":"0.00000001","discount":0,
				"overrides":[{"min_prompt_tokens":272000,"prompt":"0.000003","completion":"0.000004"}]}},
			{"name":"p2","pricing":{"prompt":"0.0000002","completion":0.000001,"discount":0.5}}]}}`,
		// The tiers of a model on OpenRouter (2026-10): a provider's endpoints with tagged variants.
		"/models/a/routed/endpoints": `{"data":{"endpoints":[
			{"name":"o","tag":"openai","pricing":{"prompt":"0.000002","completion":"0.00001"}},
			{"name":"of","tag":"openai/fast","pricing":{"prompt":"0.000004","completion":"0.00002"}},
			{"name":"ou","tag":"openai/ultrafast","pricing":{"prompt":"0.000012","completion":"0.00006"}},
			{"name":"az","tag":"azure","pricing":{"prompt":"0.000003","completion":"0.00003"}},
			{"name":"ae","tag":"azure/eu","pricing":{"prompt":"0.0000022","completion":"0.000011"}}]}}`,
		"/models/a/badtier/endpoints": `{"data":{"endpoints":[{"name":"x","pricing":{"prompt":"0.1","completion":"0.1","overrides":[{"prompt":"-3"}]}}]}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL+"/", "a/ok", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Prompt != 0.0000015 || p.Completion != 0.000004 || p.Reasoning != 0.000003 || p.Request != 0.0001 || p.Endpoints != 2 {
		t.Fatalf("pricing %+v, want the highest of each price", p)
	}
	prices := p.Prices()
	if prices.InputPerMillion != 1.5 || prices.OutputPerMillion != 7 || prices.PerRequest != 0.0001 {
		t.Fatalf("prices %+v", prices)
	}

	// Prompts of at most 100k tokens never reach p1's tier (from 272k tokens): the highest prices
	// are p2's, prompt 2e-7 and completion 1e-6 (a number); the discount and the unused prices
	// change nothing.
	real, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL, "a/real", 100_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if real.Prompt != 0.0000002 || real.Completion != 0.000001 || real.Reasoning != 0 || real.Request != 0 {
		t.Fatalf("pricing %+v", real)
	}
	// Per million in decimal: 2e-7 per token is 0.2, not 0.19999999999999998.
	if got := real.Prices(); got.InputPerMillion != 0.2 || got.OutputPerMillion != 1 {
		t.Fatalf("prices %+v", got)
	}
	// Without a bound every tier counts.
	all, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL, "a/real", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if all.Prompt != 0.000003 || all.Completion != 0.000004 {
		t.Fatalf("pricing with every tier %+v", all)
	}

	// Routing prices only the allowed endpoints. "openai" admits its variants (a priority tier
	// included) and "ignore" excludes exactly the tag it names.
	only := &llm.Routing{Only: []string{"openai", "azure/eu"}, Ignore: []string{"openai/ultrafast"}}
	routed, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL, "a/routed", 0, only)
	if err != nil {
		t.Fatal(err)
	}
	if routed.Prompt != 0.000004 || routed.Completion != 0.00002 || strings.Join(routed.Priced, ",") != "openai,openai/fast,azure/eu" {
		t.Fatalf("routed pricing %+v, want the highest of openai, openai/fast and azure/eu", routed)
	}
	unrouted, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL, "a/routed", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unrouted.Completion != 0.00006 || len(unrouted.Priced) != 0 {
		t.Fatalf("unrouted pricing %+v, want the highest of every endpoint", unrouted)
	}
	for name, tc := range map[string]struct {
		model   string
		routing *llm.Routing
		want    string
	}{
		"unknown name":    {"a/routed", &llm.Routing{Only: []string{"openia"}}, `routing names "openia"`},
		"unknown ignore":  {"a/routed", &llm.Routing{Ignore: []string{"openai/turbo"}}, `routing names "openai/turbo"`},
		"nothing allowed": {"a/routed", &llm.Routing{Only: []string{"azure/eu"}, Ignore: []string{"azure/eu"}}, "allows none"},
		"no tag":          {"a/ok", &llm.Routing{Only: []string{"openai"}}, "has no tag"},
	} {
		if _, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL, tc.model, 0, tc.routing); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}

	for model, want := range map[string]string{
		"a/badtier":  "(override)\" of a/badtier has the prompt price",
		"a/none":     "no endpoint",
		"a/nocomp":   "no completion price",
		"a/variable": "not a non-negative number",
		"a/free":     "listed as free",
		"a/junk":     "invalid character",
		"a/missing":  "HTTP 404",
		"no-slash":   "not author/slug",
	} {
		if _, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL, model, 0, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", model, err, want)
		}
	}
}
