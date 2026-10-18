//go:build cgo

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeMetrics(t *testing.T) {
	state := filepath.Join(t.TempDir(), "budget.json")
	month := time.Now().UTC().Format("2006-01")
	if err := os.WriteFile(state, []byte(`{"month":"`+month+`","spent_usd":1.25}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `
[[strategy]]
type = "llm"
name = "ai"

[strategy.llm]
type = "command"
command = ["true"]
max_tokens = 1000

[strategy.llm.budget]
monthly_usd = 20
input_usd_per_million = 2
output_usd_per_million = 10
state = "` + state + `"
`
	srv := httptest.NewServer(newTestHandler(t, testToken, cfg))
	defer srv.Close()

	get := func(token string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/metrics", nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp, string(readAll(t, resp))
	}

	if resp, _ := get(""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("metrics without the token: status %d", resp.StatusCode)
	}
	resp, body := get(testToken)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{
		"# TYPE graft_resolve_ai_spend_usd gauge\n",
		`graft_resolve_ai_spend_usd{strategy="ai"} 1.25` + "\n",
		`graft_resolve_ai_budget_usd{strategy="ai"} 20` + "\n",
		"# TYPE graft_resolve_ai_budget_exhausted_total counter\n",
		`graft_resolve_ai_budget_exhausted_total{strategy="ai"} 0` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics lack %q:\n%s", want, body)
		}
	}
}

func TestServeMetricsWithoutBudget(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, "", ""))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body := string(readAll(t, resp))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "# TYPE graft_resolve_ai_spend_usd gauge") || strings.Contains(body, "{strategy=") {
		t.Fatalf("status %d:\n%s", resp.StatusCode, body)
	}
}
