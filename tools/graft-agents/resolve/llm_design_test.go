package resolve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

// Tests of the design step (PLAN10 15.10): the symmetric context, the way out, the one repair round
// and the composition report. The provider is always a fake.

// designFile is a file with one conflicting region at line 5 (ours and theirs change it).
func designFile() LLMFile {
	base := numbered("line", 30)
	ours := strings.Replace(base, "line 5\n", "o5\n", 1)
	theirs := strings.Replace(base, "line 5\n", "t5\n", 1)
	return LLMFile{Path: "x.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs), TheirsMessage: "theirs did that"}
}

func requestNonce(t testing.TB, req llm.Request) string {
	t.Helper()
	m := patchNonceRE.FindStringSubmatch(req.Prompt)
	if m == nil {
		t.Fatalf("no nonce in the request:\n%s", req.Prompt)
	}
	return m[1]
}

// rawPatched is an answer with the patched code as it is, between the blocks of the request's nonce.
func rawPatched(nonce, code string) *llm.Response {
	return llmRawAnswer("===== BEGIN PATCHED CODE " + nonce + " =====\n" + code + "===== END PATCHED CODE " + nonce + " =====\n")
}

// TestLLMRequestHoldsWhatTheMeasurementsKept: the request of a region is THEIRS' message, the PATCH and
// the CODE with the other regions for reference, and nothing of the elements that the measurements of
// PLAN10 15.11 rejected (OURS' change and message, the way out of an abstain block). A model that writes an
// abstain block has given no answer.
func TestLLMRequestHoldsWhatTheMeasurementsKept(t *testing.T) {
	f := designFile()
	f.TheirsMessage = strings.Repeat("t", 5000)
	p := llmFixed("merged\n")
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{}); err != nil {
		t.Fatal(err)
	}
	req := p.reqs[0]
	nonce := requestNonce(t, req)
	if !strings.Contains(patchSection(t, req.Prompt, "PATCH", nonce), "-line 5\n+t5\n") {
		t.Fatal("the PATCH is THEIRS' diff")
	}
	if !strings.Contains(req.System, "THEIRS COMMIT MESSAGE (cut to the first 4 KiB)") {
		t.Fatalf("THEIRS' message is cut and marked:\n%s", req.System)
	}
	for _, part := range []string{req.System, req.Prompt} {
		for _, bad := range []string{"OURS CHANGE", "OURS COMMIT MESSAGE", "What OURS meant", "ABSTAIN", "contradict each other"} {
			if strings.Contains(part, bad) {
				t.Fatalf("%q in the request", bad)
			}
		}
	}
	abstain := &llmFake{answer: func(_ int, req llm.Request) (*llm.Response, error) {
		n := requestNonce(t, req)
		return llmRawAnswer("===== BEGIN ABSTAIN " + n + " =====\nthey contradict\n===== END ABSTAIN " + n + " =====\n"), nil
	}}
	if _, err := ResolveLLM(context.Background(), abstain, designFile(), LLMOptions{}); !errors.Is(err, ErrLLMRejected) || !strings.Contains(err.Error(), "no patched code") {
		t.Fatalf("an abstain block is no answer: %v", err)
	}
}

// TestLLMDesignRepairOnTheRulesOfAnAnswer: an answer that breaks a rule (here the anchors) is
// asked for once more, with the rule's own words and the previous answer; a second failure rejects
// the region.
func TestLLMDesignRepairOnTheRulesOfAnAnswer(t *testing.T) {
	f := designFile()
	finding := "the patched code does not start with the context lines of the code"
	bad := func(req llm.Request) *llm.Response { return rawPatched(requestNonce(t, req), "merged\n") }

	good := &llmFake{answer: func(i int, req llm.Request) (*llm.Response, error) {
		if i == 0 {
			return bad(req), nil
		}
		return llmFakeAnswer("merged\n"), nil
	}}
	stats := &LLMRegionStats{}
	res, err := ResolveLLM(WithLLMRegionStats(context.Background(), stats), good, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Calls != 2 || res.Repairs != 1 || len(good.reqs) != 2 {
		t.Fatalf("calls %d, repairs %d, requests %d", res.Calls, res.Repairs, len(good.reqs))
	}
	second := good.reqs[1]
	nonce := requestNonce(t, second)
	if second.System != good.reqs[0].System {
		t.Fatal("the repair round has its own system message")
	}
	if got := patchSection(t, second.Prompt, "YOUR PREVIOUS ANSWER", nonce); !strings.Contains(got, "BEGIN PATCHED CODE "+nonce+" =====\nmerged\n") {
		t.Fatalf("the previous answer is shown as the model gave it: %q", got)
	}
	if !strings.Contains(second.Prompt, "- "+finding+"\n") {
		t.Fatalf("the finding is not quoted exactly:\n%s", second.Prompt)
	}
	if !strings.Contains(string(res.Content), "merged") {
		t.Fatalf("content %q", res.Content)
	}
	if calls, repairs := stats.Calls(); calls != 2 || repairs != 1 {
		t.Fatalf("stats calls %d repairs %d", calls, repairs)
	}

	// Still wrong after the round: rejected, with both raw answers kept for the audit.
	still := &llmFake{answer: func(_ int, req llm.Request) (*llm.Response, error) { return bad(req), nil }}
	stats = &LLMRegionStats{}
	_, err = ResolveLLM(WithLLMRegionStats(context.Background(), stats), still, f, LLMOptions{})
	if !errors.Is(err, ErrLLMRejected) || !strings.Contains(err.Error(), "region 1") || len(still.reqs) != 2 {
		t.Fatalf("err = %v after %d calls, want rejected after one repair round", err, len(still.reqs))
	}
	if raw := stats.Rejected(); len(raw) != 1 || !strings.Contains(raw[0], "repair answer") {
		t.Fatalf("rejected %q", raw)
	}

	// No block at all is a finding too.
	nothing := &llmFake{answer: func(int, llm.Request) (*llm.Response, error) { return llmRawAnswer("I will not."), nil }}
	if _, err := ResolveLLM(context.Background(), nothing, f, LLMOptions{}); !errors.Is(err, ErrLLMRejected) || len(nothing.reqs) != 2 {
		t.Fatalf("err = %v after %d calls", err, len(nothing.reqs))
	}
	if !strings.Contains(nothing.reqs[1].Prompt, "- the answer has no patched code\n") {
		t.Fatalf("the finding of an answer without a block:\n%s", nothing.reqs[1].Prompt)
	}
}

// compositionFile has one conflicting region: OURS changes line 5, THEIRS the line next to it, so
// that the two changes do not contest each other and a good answer keeps both.
func compositionFile() LLMFile {
	base := numbered("line", 12)
	return LLMFile{Path: "x.txt", Base: []byte(base),
		Ours:   []byte(strings.Replace(base, "line 5\n", "O5\n", 1)),
		Theirs: []byte(strings.Replace(base, "line 6\n", "T6\n", 1))}
}

// TestLLMDesignCompositionReportsAndNeverRepairs: an answer that the rules and the syntax accept is never
// asked for again, whatever the composition check finds in it (PLAN10 15.11: the repair of an accepted
// answer made it worse in the paired data): the drop is reported in the check's words and the answer stays.
// A broken answer is still repaired once, and the report says how many regions were.
func TestLLMDesignCompositionReportsAndNeverRepairs(t *testing.T) {
	f := compositionFile()
	drop := `drops THEIRS' change at base line 6: the line "T6" it adds is missing`

	only := &llmFake{answer: func(int, llm.Request) (*llm.Response, error) { return llmFakeAnswer("O5\nline 6\n"), nil }}
	res, err := ResolveLLM(context.Background(), only, f, LLMOptions{})
	if err != nil || len(only.reqs) != 1 || res.Repairs != 0 || !strings.Contains(strings.Join(res.Report, "\n"), "region 1: "+drop) || !strings.Contains(string(res.Content), "O5\nline 6\n") {
		t.Fatalf("err %v, calls %d, repairs %d, report %q", err, len(only.reqs), res.Repairs, res.Report)
	}

	good := &llmFake{answer: func(int, llm.Request) (*llm.Response, error) { return llmFakeAnswer("O5\nT6\n"), nil }}
	res, err = ResolveLLM(context.Background(), good, f, LLMOptions{})
	if err != nil || len(good.reqs) != 1 || fmt.Sprint(res.Report) != "[keeps every uncontested change of both sides (2)]" {
		t.Fatalf("err %v, calls %d, report %q", err, len(good.reqs), res.Report)
	}

	brokenFirst := &llmFake{answer: func(i int, _ llm.Request) (*llm.Response, error) {
		if i == 0 {
			return llmFakeAnswer("<<<<<<< markers are not an answer\n"), nil
		}
		return llmFakeAnswer("O5\nT6\n"), nil
	}}
	res, err = ResolveLLM(context.Background(), brokenFirst, f, LLMOptions{})
	if err != nil || len(brokenFirst.reqs) != 2 || res.Repairs != 1 || !strings.Contains(string(res.Content), "O5\nT6\n") {
		t.Fatalf("a broken answer was not repaired: err %v, calls %d", err, len(brokenFirst.reqs))
	}
	want := []string{"keeps every uncontested change of both sides (2)", "1 of 1 regions were answered again after findings of the deterministic checks"}
	if fmt.Sprint(res.Report) != fmt.Sprint(want) {
		t.Fatalf("report %q, want %q", res.Report, want)
	}
}

// TestLLMComposition: the check on regions, per case of its definition (llm_compose.go).
func TestLLMComposition(t *testing.T) {
	region := func(base, ours, theirs string) *llmRegion {
		return &llmRegion{start: 10, base: base, ours: ours, theirs: theirs}
	}
	for _, tc := range []struct {
		name            string
		r               *llmRegion
		answer          string
		problems        []string
		kept, contested int
		neither         bool
	}{
		{"keeps both uncontested changes", region("a\nb\n", "A\nb\n", "a\nB\n"), "A\nB\n", nil, 2, 0, false},
		{"drops theirs", region("a\nb\n", "A\nb\n", "a\nB\n"), "A\nb\n",
			[]string{`drops THEIRS' change at base line 12: the line "B" it adds is missing`}, 1, 0, false},
		{"drops ours", region("a\nb\n", "A\nb\n", "a\nB\n"), "a\nB\n",
			[]string{`drops OURS' change at base line 11: the line "A" it adds is missing`}, 1, 0, false},
		{"a deletion that is still there", region("a\nb\nc\n", "a\nc\n", "a\nb\nC\n"), "a\nb\nC\n",
			[]string{`drops OURS' deletion at base line 12: the line "b" it deletes is still there`}, 1, 0, false},
		{"the same lines edited is contested, never a drop", region("a\n", "O\n", "T\n"), "something else\n", nil, 0, 2, true},
		{"an insertion at the point of the other's edit is contested", region("a\nb\n", "a\nx\nb\n", "a\nB\n"), "a\nB\n", nil, 0, 2, false},
		{"duplicates a line both sides added", region("a\n", "a\nnew\n", "a\nnew\nmore\n"), "a\nnew\nnew\nmore\n",
			[]string{`duplicates a line both sides added: "new"`}, 1, 1, false},
		{"blank lines and trailing whitespace do not count", region("a\nb\n", "A\nb\n", "a\nB\n"), "A  \n\n  B\r\n", nil, 2, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := llmCompose(tc.r, tc.r.start, tc.answer)
			if fmt.Sprint(got.problems) != fmt.Sprint(tc.problems) {
				t.Errorf("problems %q, want %q", got.problems, tc.problems)
			}
			if got.neither != tc.neither && tc.name == "the same lines edited is contested, never a drop" {
				t.Errorf("neither = %v", got.neither)
			}
			if tc.problems == nil && (got.kept != tc.kept || got.contested != tc.contested) {
				t.Errorf("kept %d contested %d, want %d %d", got.kept, got.contested, tc.kept, tc.contested)
			}
		})
	}
}

// TestLLMReportWording: the report's lines, as the agent reads them.
func TestLLMReportWording(t *testing.T) {
	comps := []llmComposition{
		{problems: []string{`drops THEIRS' change at base line 6: the line "T6" it adds is missing`}, contested: 1, neither: true},
		{kept: 2, contested: 3},
	}
	want := []string{
		`region 1: drops THEIRS' change at base line 6: the line "T6" it adds is missing`,
		"region 1: the answer has lines found in neither side",
		"4 contested changes resolved freely: nothing line-level to verify",
	}
	if got := llmReport(comps, 0); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("report %q, want %q", got, want)
	}
	if got := llmReport([]llmComposition{{kept: 3}}, 0); fmt.Sprint(got) != "[keeps every uncontested change of both sides (3)]" {
		t.Fatalf("report %q", got)
	}
	var many []llmComposition
	for i := 0; i < 30; i++ {
		many = append(many, llmComposition{problems: []string{"p"}})
	}
	if got := llmReport(many, 0); len(got) != 16 || !strings.HasSuffix(got[15], "more lines of this report") {
		t.Fatalf("a long report is cut: %d lines, last %q", len(got), got[len(got)-1])
	}
}

// TestLLMDesignAtTheResolver: a model answer that breaks the rules of an answer is an attempt of the
// rejected kind, the composition report goes with the resolution, and the options of the ablations are gone.
func TestLLMDesignAtTheResolver(t *testing.T) {
	file := protocol.FileConflict{
		Path: "x.txt", Kind: protocol.KindContent,
		Base:   wp19Side(protocol.ModeFile, numbered("line", 12)),
		Ours:   wp19Side(protocol.ModeFile, strings.Replace(numbered("line", 12), "line 5\n", "O5\n", 1)),
		Theirs: wp19Side(protocol.ModeFile, strings.Replace(numbered("line", 12), "line 6\n", "T6\n", 1)),
	}
	wp19SetAnswer(func(_ context.Context, req llm.Request) (*llm.Response, error) {
		return llmRawAnswer("I will not."), nil
	})
	r := wp19Build(t, wp19LLMConfig)
	_, err := r.Resolve(context.Background(), wp19Request(file))
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) || len(unresolved.Failures) != 1 {
		t.Fatalf("err = %v", err)
	}
	attempts := unresolved.Failures[0].Attempts
	if len(attempts) != 1 || attempts[0].Kind != AttemptRejected || !errors.Is(attempts[0].Err, ErrLLMRejected) {
		t.Fatalf("attempts %+v, want one rejection", attempts)
	}

	// The report goes with the resolution, and a model's resolution is marked as one.
	wp19SetAnswer(func(_ context.Context, req llm.Request) (*llm.Response, error) {
		return wp19Answer("O5\nT6\n"), nil
	})
	r = wp19Build(t, wp19LLMConfig)
	resp, err := r.Resolve(context.Background(), wp19Request(file))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Files[0]
	if got.Origin != protocol.OriginModel || !got.Proposal || fmt.Sprint(got.Report) != "[keeps every uncontested change of both sides (2)]" {
		t.Fatalf("file %+v", got)
	}

	// The elements that the measurements rejected, or made the rule, are no options any more.
	for _, gone := range []string{"symmetric_context", "abstain", "repair", "repair_composition", "git_regions", "max_context_bytes"} {
		value := "false"
		if gone == "max_context_bytes" {
			value = "4096"
		}
		if _, err := wp19BuildErr(strings.Replace(wp19LLMConfig, `paths = ["*.txt", "**/*.txt"]`, "paths = [\"*.txt\"]\n"+gone+" = "+value, 1)); err == nil {
			t.Errorf("the option %s is still accepted", gone)
		}
	}
}

// wp19BuildStrategy builds the resolver of config and returns its llm strategy.
func wp19BuildStrategy(t *testing.T, config string) *llmStrategy {
	t.Helper()
	r := wp19Build(t, config)
	for _, b := range r.bindings {
		if s, ok := b.Strategy.(*llmStrategy); ok {
			return s
		}
	}
	t.Fatal("no llm strategy in the resolver")
	return nil
}
