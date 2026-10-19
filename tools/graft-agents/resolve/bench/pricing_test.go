package bench

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	p, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL+"/", "a/ok", 0)
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
	real, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL, "a/real", 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if real.Prompt != 0.0000002 || real.Completion != 0.000001 || real.Reasoning != 0 || real.Request != 0 {
		t.Fatalf("pricing %+v", real)
	}
	// Without a bound every tier counts.
	all, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL, "a/real", 0)
	if err != nil {
		t.Fatal(err)
	}
	if all.Prompt != 0.000003 || all.Completion != 0.000004 {
		t.Fatalf("pricing with every tier %+v", all)
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
		if _, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL, model, 0); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", model, err, want)
		}
	}
}
