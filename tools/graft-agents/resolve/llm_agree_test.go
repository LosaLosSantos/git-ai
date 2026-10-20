//go:build cgo

package resolve

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

// The tests of the agree_with gate (PLAN10 15.13): two models resolve each file with the prompt of
// record, and the resolution is held back as a proposal unless they agree.

// agreeB is the provider handed out for the agree_with table of the tests.
var agreeB = &wp19Fake{answer: func(context.Context, llm.Request) (*llm.Response, error) {
	return wp19Answer("unused\n"), nil
}}

func init() {
	llm.Register("wp19-agree-b", func(llm.Config) (llm.Provider, error) { return agreeB, nil })
}

func agreeSetB(fn func(ctx context.Context, req llm.Request) (*llm.Response, error)) *wp19Fake {
	agreeB = &wp19Fake{answer: fn}
	return agreeB
}

const agreeConfig = wp19LLMConfig + `
[strategy.llm.agree_with]
type = "wp19-agree-b"
model = "model-b"
max_tokens = 256

[strategy.llm.agree_with.routing]
only = ["z-ai/fp8"]
`

func agreeAnswer(text string) func(context.Context, llm.Request) (*llm.Response, error) {
	return func(context.Context, llm.Request) (*llm.Response, error) { return wp19Answer(text), nil }
}

func agreeFail(msg string) func(context.Context, llm.Request) (*llm.Response, error) {
	return func(context.Context, llm.Request) (*llm.Response, error) { return nil, errors.New(msg) }
}

// resolveOne resolves a.txt with the agree config and returns the file the response holds.
func resolveOne(t *testing.T) (protocol.ResolvedFile, error) {
	t.Helper()
	r := wp19Build(t, agreeConfig)
	resp, err := r.Resolve(context.Background(), wp19Request(wp19Conflict("a.txt")))
	if err != nil {
		return protocol.ResolvedFile{}, err
	}
	if len(resp.Files) != 1 {
		t.Fatalf("files = %+v", resp.Files)
	}
	return resp.Files[0], nil
}

func TestAgreeWithTwoModelsThatAgreeAreNotHeld(t *testing.T) {
	a := wp19SetAnswer(agreeAnswer("merged\n"))
	b := agreeSetB(agreeAnswer("merged\n"))
	got, err := resolveOne(t)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hold != "" || string(got.Content) != "merged\n" || got.Origin != protocol.OriginModel || !got.Proposal {
		t.Fatalf("file = %+v", got)
	}
	if a.calls() != 1 || b.calls() != 1 {
		t.Fatalf("calls = %d, %d", a.calls(), b.calls())
	}
	// Both models are asked with the same prompt of record.
	nonce := regexp.MustCompile("[0-9a-f]{24}")
	strip := func(r llm.Request) string { return nonce.ReplaceAllString(r.System+"\n"+r.Prompt, "NONCE") }
	if strip(a.lastReq()) != strip(b.lastReq()) {
		t.Fatal("the two models were not given the same prompt")
	}
}

func TestAgreeWithNormalizesLikeTheJudge(t *testing.T) {
	wp19SetAnswer(agreeAnswer("merged\n"))
	agreeSetB(agreeAnswer("merged  \n\n"))
	got, err := resolveOne(t)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hold != "" {
		t.Fatalf("trailing whitespace and blank lines hold the file: %q", got.Hold)
	}
}

func TestAgreeWithDisagreementHoldsTheFirstModelsProposal(t *testing.T) {
	wp19SetAnswer(agreeAnswer("merged\n"))
	b := agreeSetB(agreeAnswer("something else\n"))
	got, err := resolveOne(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Hold, "held: the second model (model-b) disagreed") || !strings.Contains(got.Hold, "differently") {
		t.Fatalf("hold = %q", got.Hold)
	}
	if string(got.Content) != "merged\n" || got.Origin != protocol.OriginModel || !got.Proposal {
		t.Fatalf("the first model's proposal is not what is held: %+v", got)
	}
	if last := got.Report[len(got.Report)-1]; last != got.Hold {
		t.Fatalf("report = %v", got.Report)
	}
	if b.calls() != 1 {
		t.Fatalf("calls of the second model = %d", b.calls())
	}
}

func TestAgreeWithASecondModelThatFailsHolds(t *testing.T) {
	wp19SetAnswer(agreeAnswer("merged\n"))
	agreeSetB(agreeFail("provider down"))
	got, err := resolveOne(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Hold, "held: ") || !strings.Contains(got.Hold, "gave no usable answer") || !strings.Contains(got.Hold, "provider down") {
		t.Fatalf("hold = %q", got.Hold)
	}
	if string(got.Content) != "merged\n" {
		t.Fatalf("content = %q", got.Content)
	}
}

func TestAgreeWithAFirstModelThatFailsFailsTheFile(t *testing.T) {
	wp19SetAnswer(agreeFail("provider down"))
	// The second model is slow: it answers only when its context ends. The first model failing cancels
	// it, and the file is not kept waiting for it.
	var cancelled atomic.Bool
	b := agreeSetB(func(ctx context.Context, req llm.Request) (*llm.Response, error) {
		select {
		case <-ctx.Done():
			cancelled.Store(true)
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return wp19Answer("merged\n"), nil
		}
	})
	start := time.Now()
	_, err := resolveOne(t)
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the file waited %s for the second model", time.Since(start))
	}
	// The second model was asked at the same time, its call was cancelled, and it was waited for.
	if b.calls() != 1 || !cancelled.Load() {
		t.Fatalf("calls of the second model = %d, cancelled = %v", b.calls(), cancelled.Load())
	}
}

func TestAgreeWithAnUncleanReportHoldsEvenWhenTheModelsAgree(t *testing.T) {
	r := wp19Build(t, agreeConfig)
	s := r.bindings[0].Strategy.(*llmStrategy)
	clean := LLMResult{Content: []byte("x\n"), Report: []string{"keeps every uncontested change of both sides (2)"}}
	dirty := LLMResult{Content: []byte("x\n"), Report: []string{"region 1: drops theirs' change: the line \"y\" it adds is missing"}, Findings: 1}
	repeats := LLMResult{Content: []byte("x\n"), Report: []string{"region 2: duplicates a line both sides added: \"z\""}, Findings: 1}
	run := func(res LLMResult) *agreeRun { return &agreeRun{res: res} }

	if why := s.agreement(clean, run(clean)); why != "" {
		t.Fatalf("clean reports and the same file are held: %q", why)
	}
	for name, c := range map[string]struct {
		first  LLMResult
		second LLMResult
	}{
		"first drops":      {dirty, clean},
		"second drops":     {clean, dirty},
		"first duplicates": {repeats, clean},
		"both drop":        {dirty, dirty},
	} {
		if why := s.agreement(c.first, run(c.second)); !strings.HasPrefix(why, "held: a composition report was not clean") {
			t.Fatalf("%s: not held: %q", name, why)
		}
	}
	if why := s.agreement(clean, &agreeRun{err: ErrLLMNotApplicable}); !strings.Contains(why, "could not take") {
		t.Fatalf("not applicable: %q", why)
	}
}

func TestAgreeWithCountsTheCallsOfBothModels(t *testing.T) {
	wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		return &llm.Response{Text: "merged\n", Model: "a", Usage: llm.Usage{InputTokens: 10, OutputTokens: 1}, Provider: "pa"}, nil
	})
	agreeSetB(func(context.Context, llm.Request) (*llm.Response, error) {
		return &llm.Response{Text: "merged\n", Model: "b", Usage: llm.Usage{InputTokens: 20, OutputTokens: 2}, Provider: "pb"}, nil
	})
	stats := &LLMRegionStats{}
	r := wp19Build(t, agreeConfig)
	if _, err := r.Resolve(WithLLMRegionStats(context.Background(), stats), wp19Request(wp19Conflict("a.txt"))); err != nil {
		t.Fatal(err)
	}
	if u := stats.Usage(); u.InputTokens != 30 || u.OutputTokens != 3 {
		t.Fatalf("usage = %+v", u)
	}
	if calls, _ := stats.Calls(); calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
	if p := stats.Providers(); p["pa"] != 1 || p["pb"] != 1 {
		t.Fatalf("providers = %v", p)
	}
}

func TestAgreeWithConfig(t *testing.T) {
	for name, c := range map[string]struct {
		config string
		want   string
	}{
		"agree_with of agree_with": {
			agreeConfig + "\n[strategy.llm.agree_with.agree_with]\ntype = \"wp19-agree-b\"\n",
			"unknown keys",
		},
		"an unknown key": {
			strings.Replace(agreeConfig, "max_tokens = 256\n", "max_tokens = 256\nmodle = \"x\"\n", 1),
			"llm.agree_with: unknown keys",
		},
		"a budget for the first model only": {
			strings.Replace(wp19LLMConfig, "[strategy.llm.options]", "[strategy.llm.budget]\nmonthly_usd = 5\ninput_usd_per_million = 1\noutput_usd_per_million = 1\nstate = \"/tmp/x-agree-budget.json\"\n\n[strategy.llm.options]", 1) + "\n[strategy.llm.agree_with]\ntype = \"wp19-agree-b\"\nmodel = \"model-b\"\nmax_tokens = 256\n",
			"go together",
		},
		"no provider type": {
			wp19LLMConfig + "\n[strategy.llm.agree_with]\nmodel = \"model-b\"\n",
			"type is required",
		},
	} {
		_, err := wp19BuildErr(c.config)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	// Without the table the gate is off: one model, no hold.
	wp19SetAnswer(agreeAnswer("merged\n"))
	r := wp19Build(t, wp19LLMConfig)
	if r.bindings[0].Strategy.(*llmStrategy).agree != nil {
		t.Fatal("the gate is on without agree_with")
	}
}

// TestLLMFindingsSurviveTheCutOfTheReport: the report is cut at 16 lines, so a drop in a late region can
// be missing from its text, and the agree_with gate must not read the text. Findings counts them all.
func TestLLMFindingsSurviveTheCutOfTheReport(t *testing.T) {
	var comps []llmComposition
	for i := 0; i < 17; i++ {
		comps = append(comps, llmComposition{neither: true})
	}
	late := llmComposition{problems: []string{"drops THEIRS' change at base line 6: the line \"T6\" it adds is missing"}}
	comps = append(comps, late)

	report := llmReport(comps, 0)
	if len(report) != 16 {
		t.Fatalf("the report is not cut: %d lines", len(report))
	}
	for _, line := range report {
		if strings.Contains(line, "drops") {
			t.Fatalf("the late drop is in the first 16 lines, the test proves nothing: %v", report)
		}
	}
	if got := llmFindings(comps); got != 1 {
		t.Fatalf("findings = %d, want the late drop", got)
	}

	// The gate holds a file whose report text is clean and whose findings are not.
	r := wp19Build(t, agreeConfig)
	s := r.bindings[0].Strategy.(*llmStrategy)
	text := LLMResult{Content: []byte("x\n"), Report: report, Findings: llmFindings(comps)}
	clean := LLMResult{Content: []byte("x\n"), Report: report}
	if why := s.agreement(text, &agreeRun{res: clean}); !strings.HasPrefix(why, "held: a composition report was not clean") {
		t.Fatalf("a drop past the cut is not held: %q", why)
	}
	if why := s.agreement(clean, &agreeRun{res: text}); !strings.HasPrefix(why, "held: a composition report was not clean") {
		t.Fatalf("a drop past the cut of the second model is not held: %q", why)
	}
	if why := s.agreement(clean, &agreeRun{res: clean}); why != "" {
		t.Fatalf("two answers without findings are held: %q", why)
	}
}

func TestLLMNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"a\r\nb  \r\n\r\n": "a\nb",
		"a\n\n\n":          "a",
		"  a\t\n":          "  a",
		"":                 "",
		"\n":               "",
	} {
		if got := string(LLMNormalize([]byte(in))); got != want {
			t.Errorf("LLMNormalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAgreeWithNeedsAServerThatHolds: with agree_with a file may carry a hold, which only a server with
// the capability "hold" never lands. A server that proposes but cannot hold gets no model's resolution
// (the models are not called), and one that holds does.
func TestAgreeWithNeedsAServerThatHolds(t *testing.T) {
	a := wp19SetAnswer(agreeAnswer("merged\n"))
	b := agreeSetB(agreeAnswer("merged\n"))
	r := wp19Build(t, agreeConfig)

	noHold := wp19Request(wp19Conflict("a.txt"))
	noHold.Capabilities = []string{protocol.CapabilityOrigin, protocol.CapabilityProposal, protocol.CapabilityVerify}
	_, err := r.Resolve(context.Background(), noHold)
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) || !strings.Contains(err.Error(), "the server cannot hold a model's resolution (agree_with); upgrade the server") {
		t.Fatalf("err = %v", err)
	}
	if a.calls() != 0 || b.calls() != 0 {
		t.Fatalf("the models were called for a server that cannot hold: %d, %d", a.calls(), b.calls())
	}

	// Without agree_with the same server is enough.
	single := wp19Build(t, wp19LLMConfig)
	if _, err := single.Resolve(context.Background(), noHold); err != nil {
		t.Fatalf("a single model for a server that proposes: %v", err)
	}

	// A server of this release.
	if _, err := r.Resolve(context.Background(), wp19Request(wp19Conflict("a.txt"))); err != nil {
		t.Fatal(err)
	}
	// The first model served the single-model run above as well.
	if a.calls() != 2 || b.calls() != 1 {
		t.Fatalf("calls %d, %d", a.calls(), b.calls())
	}
}
