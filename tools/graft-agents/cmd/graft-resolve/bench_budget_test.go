//go:build cgo

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/bench"
)

// modelConfig writes a resolve.toml whose llm strategy runs a program that answers the fixture's
// conflict with the human resolution, and reports no usage.
func modelConfig(t *testing.T, work string) string {
	t.Helper()
	answer := filepath.Join(work, "answer.sh")
	// The conflict is the file's only line, so the patched code has no context: the program takes
	// the nonce of the request and answers the human line between the PATCHED CODE lines.
	script := "#!/bin/sh\nn=$(grep -o 'BEGIN PATCH [0-9a-f]*' | head -1 | cut -d' ' -f3)\n" +
		"printf '===== BEGIN PATCHED CODE %s =====\\nx = 2 + 3\\n===== END PATCHED CODE %s =====\\n' \"$n\" \"$n\"\n"
	if err := os.WriteFile(answer, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(work, "resolve.toml")
	toml := "[[strategy]]\ntype = \"llm\"\n\n[strategy.llm]\ntype = \"command\"\ncommand = [\"" + answer + "\"]\nmodel = \"openai/gpt-test\"\nmax_tokens = 1000\n"
	if err := os.WriteFile(cfg, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// openRouterFake serves the endpoints of openai/gpt-test, two of them at different prices.
func openRouterFake(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/openai/gpt-test/endpoints" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"openai/gpt-test","endpoints":[
			{"name":"a","pricing":{"prompt":"0.000002","completion":"0.00001","request":"0"}},
			{"name":"b","pricing":{"prompt":"0.000003","completion":"0.000008","internal_reasoning":"0.000001"}}]}}`))
	}))
}

func runBenchJSON(t *testing.T, args ...string) (int, bench.Report, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	jsonOut := filepath.Join(t.TempDir(), "report.json")
	code := run(append([]string{"bench", "run", "--json-out", jsonOut}, args...), nil, &stdout, &stderr)
	var rep bench.Report
	if code == 0 {
		raw, err := os.ReadFile(jsonOut)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatal(err)
		}
	}
	return code, rep, stdout.String() + stderr.String()
}

func TestBenchRunWithModelNeedsCap(t *testing.T) {
	work, _, clones, manifest := mineFixture(t)
	cfg := modelConfig(t, work)
	code, _, out := runBenchJSON(t, "--config", cfg, "--manifest", manifest, "--clones", clones)
	if code != 2 || !strings.Contains(out, "--max-usd is required") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestBenchRunPricesFromOpenRouterAndCaps(t *testing.T) {
	work, _, clones, manifest := mineFixture(t)
	cfg := modelConfig(t, work)
	api := openRouterFake(t)
	defer api.Close()

	code, rep, out := runBenchJSON(t, "--config", cfg, "--manifest", manifest, "--clones", clones,
		"--max-usd", "1", "--openrouter-api", api.URL, "--label", "model")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if rep.Total.Correct != 1 || rep.MaxUSD != 1 || rep.SpendUSD <= 0 || rep.SpendUSD > 1 {
		t.Fatalf("report %+v", rep)
	}
	// The worst case of every kind over the endpoints: prompt 3e-6, completion 1e-5,
	// internal reasoning 1e-6.
	p := rep.Pricing
	if p == nil || p.Model != "openai/gpt-test" || p.Endpoints != 2 || p.Prompt != 0.000003 || p.Completion != 0.00001 || p.Reasoning != 0.000001 {
		t.Fatalf("pricing %+v", p)
	}
	// No usage reported: the spend is the reservation, the input bytes plus 1000 output tokens
	// at $11 per million.
	if rep.SpendUSD < 1000*11e-6 {
		t.Fatalf("spend %v is below the output worst case", rep.SpendUSD)
	}

	// A cap below one call's worst case: the call is not made.
	code, rep, out = runBenchJSON(t, "--config", cfg, "--manifest", manifest, "--clones", clones,
		"--max-usd", "0.001", "--openrouter-api", api.URL)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if rep.Total.NotRun != 1 || rep.SpendUSD != 0 {
		t.Fatalf("report %+v", rep)
	}
	if !strings.Contains(out, "1 not run: the cap was reached") {
		t.Fatalf("markdown: %s", out)
	}
}

func TestBenchRunRefusesUnreadablePrice(t *testing.T) {
	work, _, clones, manifest := mineFixture(t)
	cfg := modelConfig(t, work)
	api := openRouterFake(t)
	defer api.Close()
	toml, _ := os.ReadFile(cfg)
	if err := os.WriteFile(cfg, []byte(strings.Replace(string(toml), "openai/gpt-test", "openai/unknown", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, out := runBenchJSON(t, "--config", cfg, "--manifest", manifest, "--clones", clones, "--max-usd", "1", "--openrouter-api", api.URL)
	if code != 1 || !strings.Contains(out, "pricing:") || !strings.Contains(out, "HTTP 404") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

// TestBenchAnswersAndRejudge keeps the answers of a run, then judges them again without the
// model: the same answer gives the same outcome, and a changed answer is judged anew.
func TestBenchAnswersAndRejudge(t *testing.T) {
	work, _, clones, manifest := mineFixture(t)
	cfg := modelConfig(t, work)
	answers := filepath.Join(work, "answers")
	reportIn := filepath.Join(work, "run.json")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"bench", "run", "--config", cfg, "--manifest", manifest, "--clones", clones, "--max-usd", "1",
		"--pricing", "fixed", "--input-usd-per-million", "1", "--output-usd-per-million", "1",
		"--answers-dir", answers, "--json-out", reportIn}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("run exited %d: %s", code, stderr.String())
	}
	var rep bench.Report
	raw, _ := os.ReadFile(reportIn)
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Total.Correct != 1 || rep.Results[0].Answer != "write" {
		t.Fatalf("run report %+v", rep)
	}
	stored, err := os.ReadFile(filepath.Join(answers, rep.Results[0].ID+".answer"))
	if err != nil || string(stored) != "x = 2 + 3\n" {
		t.Fatalf("stored answer %q, %v", stored, err)
	}

	rejudge := func() bench.Report {
		t.Helper()
		out := filepath.Join(t.TempDir(), "rejudged.json")
		var stdout, stderr bytes.Buffer
		if code := run([]string{"bench", "rejudge", "--report-in", reportIn, "--manifest", manifest, "--clones", clones,
			"--answers-dir", answers, "--json-out", out}, nil, &stdout, &stderr); code != 0 {
			t.Fatalf("rejudge exited %d: %s", code, stderr.String())
		}
		var r bench.Report
		raw, _ := os.ReadFile(out)
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := rejudge(); r.Total.Correct != 1 || r.SpendUSD != rep.SpendUSD || r.Label != rep.Label {
		t.Fatalf("rejudged %+v", r)
	}
	if err := os.WriteFile(filepath.Join(answers, rep.Results[0].ID+".answer"), []byte("x = 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := rejudge(); r.Total.Wrong != 1 || r.Total.Correct != 0 {
		t.Fatalf("rejudged a changed answer %+v", r.Total)
	}
}

func TestBenchRunFixedPricing(t *testing.T) {
	work, _, clones, manifest := mineFixture(t)
	cfg := modelConfig(t, work)
	code, rep, out := runBenchJSON(t, "--config", cfg, "--manifest", manifest, "--clones", clones,
		"--max-usd", "1", "--pricing", "fixed", "--input-usd-per-million", "1", "--output-usd-per-million", "1")
	if code != 0 || rep.Pricing != nil || rep.Total.Correct != 1 {
		t.Fatalf("exit %d, report %+v: %s", code, rep, out)
	}
	code, _, out = runBenchJSON(t, "--config", cfg, "--manifest", manifest, "--clones", clones, "--max-usd", "1", "--pricing", "fixed")
	if code != 1 || !strings.Contains(out, "no price is set") {
		t.Fatalf("fixed pricing without prices: exit %d: %s", code, out)
	}
}
