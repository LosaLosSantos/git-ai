package superfan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

type summaryFake struct {
	reqs []llm.Request
	resp *llm.Response
	err  error
}

func (f *summaryFake) Name() string { return "fake:summary" }

func (f *summaryFake) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.reqs = append(f.reqs, req)
	return f.resp, f.err
}

func summaryAnswer(summary, risk string) *llm.Response {
	b, _ := json.Marshal(map[string]string{"summary": summary, "risk": risk})
	return &llm.Response{Text: string(b), JSON: b, Model: "fake-model"}
}

func TestSummarizeDigestParsesAnswer(t *testing.T) {
	p := &summaryFake{resp: summaryAnswer("  Two lands on main, one auto-revert.  ", "high")}
	digest := []byte(`{"component":"payments","lands":2,"reverts":1}`)

	got, err := SummarizeDigest(context.Background(), p, digest, DigestSummaryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "Two lands on main, one auto-revert." || got.Risk != SummaryRiskHigh || got.Model != "fake-model" || got.Truncated {
		t.Fatalf("summary = %+v", got)
	}

	if len(p.reqs) != 1 {
		t.Fatalf("calls = %d", len(p.reqs))
	}
	req := p.reqs[0]
	if !strings.Contains(req.Prompt, string(digest)) {
		t.Fatal("prompt misses the digest")
	}
	if !strings.Contains(req.System, "Report only what the digest states") {
		t.Fatal("system prompt misses the factual rule")
	}
	if req.SchemaName == "" || req.JSONSchema == nil {
		t.Fatal("schema not requested")
	}
	enum, _ := json.Marshal(req.JSONSchema["properties"])
	if !strings.Contains(string(enum), `"critical"`) {
		t.Fatalf("risk enum missing critical: %s", enum)
	}
}

func TestSummarizeDigestFallsBackToTextJSON(t *testing.T) {
	p := &summaryFake{resp: &llm.Response{Text: "Here it is:\n```json\n{\"summary\":\"quiet\",\"risk\":\"low\"}\n```"}}

	got, err := SummarizeDigest(context.Background(), p, []byte(`{}`), DigestSummaryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "quiet" || got.Risk != SummaryRiskLow || got.Model != "fake:summary" {
		t.Fatalf("summary = %+v", got)
	}
}

func TestSummarizeDigestRejections(t *testing.T) {
	for name, resp := range map[string]*llm.Response{
		"unknown risk":   summaryAnswer("ok", "severe"),
		"empty summary":  summaryAnswer("   ", "low"),
		"oversized":      summaryAnswer(strings.Repeat("x", llmSummaryMaxBytes+1), "low"),
		"no json":        {Text: "nothing structured"},
		"bad json types": {JSON: json.RawMessage(`{"summary": 1, "risk": "low"}`)},
		"nil response":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			p := &summaryFake{resp: resp}
			_, err := SummarizeDigest(context.Background(), p, []byte(`{}`), DigestSummaryOptions{})
			if !errors.Is(err, ErrSummaryRejected) {
				t.Fatalf("err = %v, want rejection", err)
			}
		})
	}
}

func TestSummarizeDigestProviderError(t *testing.T) {
	boom := errors.New("upstream down")
	p := &summaryFake{err: boom}

	_, err := SummarizeDigest(context.Background(), p, []byte(`{}`), DigestSummaryOptions{})
	if !errors.Is(err, boom) || errors.Is(err, ErrSummaryRejected) {
		t.Fatalf("err = %v", err)
	}
}

func TestSummarizeDigestNoProvider(t *testing.T) {
	if _, err := SummarizeDigest(context.Background(), nil, []byte(`{}`), DigestSummaryOptions{}); err == nil {
		t.Fatal("nil provider accepted")
	}
}

func TestSummarizeDigestBoundsInput(t *testing.T) {
	// "é" is two bytes, so a cut at an odd offset must back off to a boundary.
	digest := []byte(strings.Repeat("é", 100))
	p := &summaryFake{resp: summaryAnswer("big", "medium")}

	got, err := SummarizeDigest(context.Background(), p, digest, DigestSummaryOptions{MaxInputBytes: 11})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated {
		t.Fatal("truncation not reported")
	}
	prompt := p.reqs[0].Prompt
	if strings.Contains(prompt, strings.Repeat("é", 10)+"é") {
		t.Fatal("digest not bounded")
	}
	if !strings.Contains(prompt, "digest truncated: 10 of 200 bytes shown") {
		t.Fatalf("prompt does not say the digest was cut:\n%s", prompt)
	}
	if !strings.Contains(prompt, "summarize what is shown") {
		t.Fatal("prompt does not ask to say so")
	}
	if !strings.HasPrefix(prompt, "Digest of one component") {
		t.Fatal("unexpected prompt head")
	}
}

func TestSummarizeDigestNotTruncatedWhenSmall(t *testing.T) {
	p := &summaryFake{resp: summaryAnswer("small", "low")}
	got, err := SummarizeDigest(context.Background(), p, []byte(`{"a":1}`), DigestSummaryOptions{MaxInputBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got.Truncated || strings.Contains(p.reqs[0].Prompt, "truncated") {
		t.Fatal("small digest marked truncated")
	}
}
