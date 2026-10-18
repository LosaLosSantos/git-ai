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

	p, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL+"/", "a/ok")
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

	for model, want := range map[string]string{
		"a/none":     "no endpoint",
		"a/nocomp":   "no completion price",
		"a/variable": "not a non-negative number",
		"a/free":     "listed as free",
		"a/junk":     "invalid character",
		"a/missing":  "HTTP 404",
		"no-slash":   "not author/slug",
	} {
		if _, err := OpenRouterPricing(context.Background(), srv.Client(), srv.URL, model); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", model, err, want)
		}
	}
}
