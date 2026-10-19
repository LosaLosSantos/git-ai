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
	return LLMFile{Path: "x.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs), TheirsMessage: "theirs did that", OursMessage: "ours did this"}
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

func rawAbstain(nonce, reason string) *llm.Response {
	return llmRawAnswer("===== BEGIN ABSTAIN " + nonce + " =====\n" + reason + "\n===== END ABSTAIN " + nonce + " =====\n")
}

func TestLLMDesignSymmetricContext(t *testing.T) {
	f := designFile()
	p := llmFixed("merged\n")
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{}); err != nil {
		t.Fatal(err)
	}
	req := p.reqs[0]
	nonce := requestNonce(t, req)
	if got := patchSection(t, req.System, "OURS COMMIT MESSAGE", nonce); got != "ours did this" {
		t.Fatalf("ours message section %q", got)
	}
	if !strings.Contains(req.System, "What OURS meant to do") || !strings.Contains(req.System, "OURS CHANGE is what OURS did") {
		t.Fatalf("the system message does not explain the symmetric context:\n%s", req.System)
	}
	ours := patchSection(t, req.Prompt, "OURS CHANGE", nonce)
	if !strings.Contains(ours, "-line 5\n+o5\n") || strings.Contains(ours, "t5") {
		t.Fatalf("OURS CHANGE is OURS' diff from the base:\n%s", ours)
	}
	if !strings.Contains(patchSection(t, req.Prompt, "PATCH", nonce), "-line 5\n+t5\n") {
		t.Fatal("the PATCH is THEIRS' diff")
	}
	// The order: the PATCH, OURS' change, the CODE.
	if i, j, k := strings.Index(req.Prompt, "BEGIN PATCH "), strings.Index(req.Prompt, "BEGIN OURS CHANGE"), strings.Index(req.Prompt, "BEGIN CODE"); !(i < j && j < k) {
		t.Fatalf("sections out of order: %d %d %d", i, j, k)
	}

	// Switched off, nothing of OURS' side is in the request: it is the request of R1b.
	q := llmFixed("merged\n")
	if _, err := ResolveLLM(context.Background(), q, f, LLMOptions{NoSymmetricContext: true}); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{q.reqs[0].System, q.reqs[0].Prompt} {
		for _, bad := range []string{"OURS CHANGE", "OURS COMMIT MESSAGE", "What OURS meant"} {
			if strings.Contains(part, bad) {
				t.Fatalf("%q in the request without the symmetric context", bad)
			}
		}
	}
	// A file with no message of OURS leaves that section out, and keeps OURS' change.
	f.OursMessage = ""
	r := llmFixed("merged\n")
	if _, err := ResolveLLM(context.Background(), r, f, LLMOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.reqs[0].System, "OURS COMMIT MESSAGE") || !strings.Contains(r.reqs[0].Prompt, "BEGIN OURS CHANGE") {
		t.Fatal("no message of OURS: no section, but its change stays")
	}
}

// TestLLMDesignSymmetricContextIsCapped: OURS' diff and the messages are cut to the cap, and the
// request says so.
func TestLLMDesignSymmetricContextIsCapped(t *testing.T) {
	base := numbered("line", 40)
	ours, theirs := base, base
	for i := 5; i <= 30; i++ {
		ours = strings.Replace(ours, fmt.Sprintf("line %d\n", i), fmt.Sprintf("ours changed line number %d with some more words\n", i), 1)
		theirs = strings.Replace(theirs, fmt.Sprintf("line %d\n", i), fmt.Sprintf("theirs changed line number %d\n", i), 1)
	}
	f := LLMFile{Path: "x.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs),
		TheirsMessage: strings.Repeat("t", 5000), OursMessage: strings.Repeat("o", 5000)}
	p := llmFixed("merged\n")
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxContextBytes: 800}); err != nil {
		t.Fatal(err)
	}
	req := p.reqs[0]
	nonce := requestNonce(t, req)
	if body := patchSection(t, req.Prompt, "OURS CHANGE", nonce); len(body) > 800+len("... (the rest of this diff is cut to keep the request short)\n") ||
		!strings.Contains(body, "the rest of this diff is cut") {
		t.Fatalf("OURS CHANGE of %d bytes is not cut:\n%s", len(body), body)
	}
	// OURS' message has its own cap; THEIRS' keeps the one it always had, with or without the symmetric
	// context: the ablation of the symmetric context changes nothing of THEIRS' side.
	if !strings.Contains(req.System, "OURS COMMIT MESSAGE (cut to the first 256 bytes)") {
		t.Fatalf("OURS' message not cut and marked:\n%s", req.System)
	}
	if !strings.Contains(req.System, "THEIRS COMMIT MESSAGE (cut to the first 4 KiB)") {
		t.Fatalf("THEIRS' message has its own cap:\n%s", req.System)
	}
}

func TestLLMDesignAbstain(t *testing.T) {
	f := designFile()
	p := &llmFake{answer: func(_ int, req llm.Request) (*llm.Response, error) {
		return rawAbstain(requestNonce(t, req), "OURS makes the value a string,\nTHEIRS makes it a number:   no   version keeps both."), nil
	}}
	stats := &LLMRegionStats{}
	_, err := ResolveLLM(WithLLMRegionStats(context.Background(), stats), p, f, LLMOptions{})
	var abstain *llmAbstainError
	if !errors.As(err, &abstain) || !errors.Is(err, ErrLLMAbstained) {
		t.Fatalf("err = %v, want the model's abstention", err)
	}
	if want := "OURS makes the value a string, THEIRS makes it a number: no version keeps both."; abstain.reason != want {
		t.Fatalf("reason %q, want %q (one line, spaces collapsed)", abstain.reason, want)
	}
	if !strings.Contains(err.Error(), "the model abstained on region 1") || stats.Abstained() != abstain.reason {
		t.Fatalf("err %q, stats %q", err, stats.Abstained())
	}
	if !strings.Contains(p.reqs[0].System, "===== BEGIN ABSTAIN "+requestNonce(t, p.reqs[0])+" =====") {
		t.Fatal("the way out is not offered")
	}

	// One abstaining region abstains the file, with the calls spent so far counted.
	two := LLMFile{Path: "two.txt", Base: []byte(numbered("line", 30)), TheirsMessage: "m"}
	two.Ours = []byte(strings.NewReplacer("line 5\n", "o5\n", "line 20\n", "o20\n").Replace(string(two.Base)))
	two.Theirs = []byte(strings.NewReplacer("line 5\n", "t5\n", "line 20\n", "t20\n").Replace(string(two.Base)))
	q := &llmFake{answer: func(i int, req llm.Request) (*llm.Response, error) {
		if i == 1 {
			return rawAbstain(requestNonce(t, req), "contradiction"), nil
		}
		return llmFakeAnswer("merged\n"), nil
	}}
	stats = &LLMRegionStats{}
	if _, err := ResolveLLM(WithLLMRegionStats(context.Background(), stats), q, two, LLMOptions{}); !errors.Is(err, ErrLLMAbstained) || !strings.Contains(err.Error(), "region 2") {
		t.Fatalf("err = %v", err)
	}
	if calls, repairs := stats.Calls(); calls != 2 || repairs != 0 {
		t.Fatalf("calls %d, repairs %d", calls, repairs)
	}
}

// TestLLMDesignAbstainReadsTheLastBlock: a model may draft before it answers; the last block of
// either kind is the answer. Without the option, an abstain block is no answer at all.
func TestLLMDesignAbstainReadsTheLastBlock(t *testing.T) {
	nonce := "0123456789abcdef"
	abstain := "===== BEGIN ABSTAIN " + nonce + " =====\nwhy\n===== END ABSTAIN " + nonce + " =====\n"
	patched := "===== BEGIN PATCHED CODE " + nonce + " =====\ncode\n===== END PATCHED CODE " + nonce + " =====\n"
	for name, tc := range map[string]struct {
		text, patched, reason string
		abstain               bool
	}{
		"abstain after patched":  {patched + abstain, "", "why", true},
		"patched after abstain":  {abstain + patched, "code\n", "", true},
		"abstain not offered":    {abstain, "", "", false},
		"abstain not offered, 2": {abstain + patched, "code\n", "", false},
	} {
		got, reason, err := llmParseAnswer(tc.text, nonce, tc.abstain)
		if tc.patched == "" && tc.reason == "" {
			if !errors.Is(err, ErrLLMRejected) {
				t.Errorf("%s: err = %v, want no patched code", name, err)
			}
			continue
		}
		if err != nil || got != tc.patched || reason != tc.reason {
			t.Errorf("%s: patched %q reason %q err %v", name, got, reason, err)
		}
	}
	if _, _, err := llmParseAnswer("===== BEGIN ABSTAIN "+nonce+" =====\nwhy\n", nonce, true); !errors.Is(err, ErrLLMRejected) || !strings.Contains(err.Error(), "no end line") {
		t.Errorf("an abstain block without its end line: %v", err)
	}
	if _, reason, _ := llmParseAnswer("===== BEGIN ABSTAIN "+nonce+" =====\n   \n===== END ABSTAIN "+nonce+" =====\n", nonce, true); reason != "no reason given" {
		t.Errorf("empty reason %q", reason)
	}
}

// TestLLMDesignRepairOnTheRulesOfAnAnswer: an answer that breaks a rule (here the anchors) is
// asked for once more, with the rule's own words and the previous answer; a second failure rejects
// the region, and without the option there is no second call.
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

	// Without the option: one call, rejected.
	none := &llmFake{answer: func(_ int, req llm.Request) (*llm.Response, error) { return bad(req), nil }}
	if _, err := ResolveLLM(context.Background(), none, f, LLMOptions{NoRepair: true}); !errors.Is(err, ErrLLMRejected) || len(none.reqs) != 1 {
		t.Fatalf("err = %v after %d calls", err, len(none.reqs))
	}

	// No block at all is a finding too, and a model that asks to abstain in the round is read.
	nothing := &llmFake{answer: func(i int, req llm.Request) (*llm.Response, error) {
		if i == 0 {
			return llmRawAnswer("I will not."), nil
		}
		return rawAbstain(requestNonce(t, req), "they contradict"), nil
	}}
	if _, err := ResolveLLM(context.Background(), nothing, f, LLMOptions{}); !errors.Is(err, ErrLLMAbstained) || len(nothing.reqs) != 2 {
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

// TestLLMDesignRepairOnTheCompositionAndTheReport: an answer that drops THEIRS' change is asked for
// once more with the drop in the check's words; the repaired answer is kept when it has fewer
// findings. The report goes with the result, and is the same words.
func TestLLMDesignRepairOnTheCompositionAndTheReport(t *testing.T) {
	f := compositionFile()
	drop := `drops THEIRS' change at base line 6: the line "T6" it adds is missing`

	p := &llmFake{answer: func(i int, _ llm.Request) (*llm.Response, error) {
		if i == 0 {
			return llmFakeAnswer("O5\nline 6\n"), nil
		}
		return llmFakeAnswer("O5\nT6\n"), nil
	}}
	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 2 || res.Repairs != 1 {
		t.Fatalf("requests %d, repairs %d", len(p.reqs), res.Repairs)
	}
	if !strings.Contains(p.reqs[1].Prompt, "- "+drop+"\n") {
		t.Fatalf("the drop is not quoted exactly:\n%s", p.reqs[1].Prompt)
	}
	if !strings.Contains(string(res.Content), "O5\nT6\n") || strings.Contains(string(res.Content), "line 6") {
		t.Fatalf("content %q", res.Content)
	}
	want := []string{"keeps every uncontested change of both sides (2)", "1 of 1 regions were answered again after findings of the deterministic checks"}
	if fmt.Sprint(res.Report) != fmt.Sprint(want) {
		t.Fatalf("report %q, want %q", res.Report, want)
	}

	// A repair that does not have fewer findings is not taken: the first answer stays, with its report.
	same := &llmFake{answer: func(i int, _ llm.Request) (*llm.Response, error) {
		if i == 0 {
			return llmFakeAnswer("O5\nline 6\n"), nil
		}
		return llmFakeAnswer("O5\nline 6\nmore\n"), nil // still drops THEIRS' change: not fewer findings
	}}
	res, err = ResolveLLM(context.Background(), same, f, LLMOptions{})
	if err != nil || len(same.reqs) != 2 {
		t.Fatalf("err %v after %d calls", err, len(same.reqs))
	}
	if !strings.Contains(string(res.Content), "O5\nline 6\n") || strings.Contains(string(res.Content), "more") || !strings.Contains(strings.Join(res.Report, "\n"), "region 1: "+drop) {
		t.Fatalf("content %q, report %q", res.Content, res.Report)
	}

	// A failed repair call keeps the first answer too: the repair is best effort once an answer is valid.
	broken := &llmFake{answer: func(i int, _ llm.Request) (*llm.Response, error) {
		if i == 0 {
			return llmFakeAnswer("O5\nline 6\n"), nil
		}
		return nil, errors.New("503")
	}}
	if res, err = ResolveLLM(context.Background(), broken, f, LLMOptions{}); err != nil || !strings.Contains(string(res.Content), "O5\nline 6\n") {
		t.Fatalf("err %v, content %q", err, res.Content)
	}

	// Without the option the drop is reported and nothing is asked again.
	none := &llmFake{answer: func(int, llm.Request) (*llm.Response, error) { return llmFakeAnswer("O5\nline 6\n"), nil }}
	res, err = ResolveLLM(context.Background(), none, f, LLMOptions{NoRepair: true})
	if err != nil || len(none.reqs) != 1 || res.Repairs != 0 || !strings.Contains(strings.Join(res.Report, "\n"), drop) {
		t.Fatalf("err %v, calls %d, report %q", err, len(none.reqs), res.Report)
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

// TestLLMDesignAtTheResolver: the abstention is an attempt of its own kind with the model's reason,
// the composition report goes with the resolution, and the options come from the configuration.
func TestLLMDesignAtTheResolver(t *testing.T) {
	file := protocol.FileConflict{
		Path: "x.txt", Kind: protocol.KindContent,
		Base:        wp19Side(protocol.ModeFile, numbered("line", 12)),
		Ours:        wp19Side(protocol.ModeFile, strings.Replace(numbered("line", 12), "line 5\n", "O5\n", 1)),
		Theirs:      wp19Side(protocol.ModeFile, strings.Replace(numbered("line", 12), "line 6\n", "T6\n", 1)),
		OursMessage: "ours changed line 5",
	}
	fake := wp19SetAnswer(func(_ context.Context, req llm.Request) (*llm.Response, error) {
		return rawAbstain(requestNonce(t, req), "they contradict"), nil
	})
	r := wp19Build(t, wp19LLMConfig)
	_, err := r.Resolve(context.Background(), wp19Request(file))
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) || len(unresolved.Failures) != 1 {
		t.Fatalf("err = %v", err)
	}
	attempts := unresolved.Failures[0].Attempts
	if len(attempts) != 1 || attempts[0].Kind != AttemptAbstained || !errors.Is(attempts[0].Err, ErrLLMAbstained) {
		t.Fatalf("attempts %+v, want one abstention", attempts)
	}
	if reasons := unresolved.Failures[0].Reasons; len(reasons) != 1 || !strings.Contains(reasons[0], "abstained on region 1: they contradict") {
		t.Fatalf("reasons %q", reasons)
	}
	if got := fake.lastReq().System; !strings.Contains(got, "OURS COMMIT MESSAGE") {
		t.Fatalf("the OURS message did not reach the model:\n%s", got)
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

	// The options of the design step come from the strategy table, on unless switched off.
	for key, check := range map[string]func(LLMOptions) bool{
		"symmetric_context": func(o LLMOptions) bool { return o.NoSymmetricContext },
		"abstain":           func(o LLMOptions) bool { return o.NoAbstain },
		"repair":            func(o LLMOptions) bool { return o.NoRepair },
		"git_regions":       func(o LLMOptions) bool { return o.NoGitRegions },
	} {
		cfg := strings.Replace(wp19LLMConfig, `paths = ["*.txt", "**/*.txt"]`, "paths = [\"*.txt\"]\n"+key+" = false", 1)
		s := wp19BuildStrategy(t, cfg)
		if !check(s.opt) {
			t.Errorf("%s = false did not switch the element off: %+v", key, s.opt)
		}
		if on := wp19BuildStrategy(t, wp19LLMConfig); check(on.opt) {
			t.Errorf("%s is off by default: %+v", key, on.opt)
		}
	}
	if _, err := wp19BuildErr(strings.Replace(wp19LLMConfig, `paths = ["*.txt", "**/*.txt"]`, "paths = [\"*.txt\"]\nrepair = \"no\"", 1)); err == nil {
		t.Error("repair = \"no\" is accepted")
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
