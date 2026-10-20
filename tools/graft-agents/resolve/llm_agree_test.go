//go:build cgo

package resolve

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

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
	if !strings.Contains(got.Hold, "model-b") || !strings.Contains(got.Hold, "differently") {
		t.Fatalf("hold = %q", got.Hold)
	}
	if string(got.Content) != "merged\n" || got.Origin != protocol.OriginModel || !got.Proposal {
		t.Fatalf("the first model's proposal is not what is held: %+v", got)
	}
	if last := got.Report[len(got.Report)-1]; !strings.Contains(last, "the second model disagreed") {
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
	if !strings.Contains(got.Hold, "gave no usable answer") || !strings.Contains(got.Hold, "provider down") {
		t.Fatalf("hold = %q", got.Hold)
	}
	if string(got.Content) != "merged\n" {
		t.Fatalf("content = %q", got.Content)
	}
}

func TestAgreeWithAFirstModelThatFailsFailsTheFile(t *testing.T) {
	wp19SetAnswer(agreeFail("provider down"))
	b := agreeSetB(agreeAnswer("merged\n"))
	_, err := resolveOne(t)
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("err = %v", err)
	}
	// The second model was asked at the same time and its call is waited for: the cost is real.
	if b.calls() != 1 {
		t.Fatalf("calls of the second model = %d", b.calls())
	}
}

func TestAgreeWithAnUncleanReportHoldsEvenWhenTheModelsAgree(t *testing.T) {
	r := wp19Build(t, agreeConfig)
	s := r.bindings[0].Strategy.(*llmStrategy)
	clean := LLMResult{Content: []byte("x\n"), Report: []string{"keeps every uncontested change of both sides (2)"}}
	dirty := LLMResult{Content: []byte("x\n"), Report: []string{"region 1: drops theirs' change: the line \"y\" it adds is missing"}}
	repeats := LLMResult{Content: []byte("x\n"), Report: []string{"region 2: duplicates a line both sides added: \"z\""}}
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
		if why := s.agreement(c.first, run(c.second)); why == "" || !strings.Contains(why, "finding") {
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

func TestLLMReportClean(t *testing.T) {
	for report, want := range map[string]bool{
		"": true,
		"keeps every uncontested change of both sides (3)":                              true,
		"2 contested changes resolved freely: nothing line-level to verify":             true,
		"1 of 2 regions were answered again after findings of the deterministic checks": true,
		"region 1: drops ours' change 3-4: the line \"a\" it adds is missing":           false,
		"region 2: duplicates a line both sides added: \"b\"":                           false,
	} {
		var lines []string
		if report != "" {
			lines = []string{report}
		}
		if got := LLMReportClean(lines); got != want {
			t.Errorf("LLMReportClean(%q) = %v", report, got)
		}
	}
	if !LLMReportClean([]string{"a", "b", "region 3: the answer has lines found in neither side"}) {
		t.Error("a line found in neither side is a free resolution, not a drop")
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
