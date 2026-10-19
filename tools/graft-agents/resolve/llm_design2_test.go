package resolve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// Tests of the review of the design step: the repair quotes the same thing whatever triggered it, is
// skipped when it would not fit, the regions are git's, and the counters count what happened.

// TestLLMRepairQuotesTheBlockWhateverTheTrigger: the previous answer of a repair is the patched code
// between the tags of its block, with the context lines of the CODE, whether a rule of an answer was
// broken (the code as the model wrote it) or a check found something in a valid region.
func TestLLMRepairQuotesTheBlockWhateverTheTrigger(t *testing.T) {
	// A rule broken: the anchors are missing, and the previous answer is what the model wrote.
	f := designFile()
	p := &llmFake{answer: func(i int, req llm.Request) (*llm.Response, error) {
		if i == 0 {
			return rawPatched(requestNonce(t, req), "merged\n"), nil
		}
		return llmFakeAnswer("merged\n"), nil
	}}
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{}); err != nil {
		t.Fatal(err)
	}
	nonce := requestNonce(t, p.reqs[1])
	got := patchSection(t, p.reqs[1].Prompt, "YOUR PREVIOUS ANSWER", nonce)
	if want := "===== BEGIN PATCHED CODE " + nonce + " =====\nmerged\n===== END PATCHED CODE " + nonce + " =====\n"; got != want {
		t.Fatalf("rule-break repair quotes %q, want %q", got, want)
	}

	// A valid region with a composition finding: the same shape, with the context lines.
	c := compositionFile()
	q := &llmFake{answer: func(i int, _ llm.Request) (*llm.Response, error) {
		if i == 0 {
			return llmFakeAnswer("O5\nline 6\n"), nil
		}
		return llmFakeAnswer("O5\nT6\n"), nil
	}}
	if _, err := ResolveLLM(context.Background(), q, c, LLMOptions{}); err != nil {
		t.Fatal(err)
	}
	nonce = requestNonce(t, q.reqs[1])
	got = patchSection(t, q.reqs[1].Prompt, "YOUR PREVIOUS ANSWER", nonce)
	if !strings.HasPrefix(got, "===== BEGIN PATCHED CODE "+nonce+" =====\nline 2\nline 3\nline 4\nO5\nline 6\n") ||
		!strings.HasSuffix(got, "line 7\nline 8\nline 9\n===== END PATCHED CODE "+nonce+" =====\n") {
		t.Fatalf("composition repair quotes %q", got)
	}
}

// TestLLMRepairIsSkippedWhenItWouldNotFit: the repair request holds the previous answer and the findings
// besides the first request, and is held to max_prompt_bytes like it. Too big: no repair call, and the
// first answer stays (or the region is rejected, if the first answer broke a rule).
func TestLLMRepairIsSkippedWhenItWouldNotFit(t *testing.T) {
	c := compositionFile()
	probe := llmFixed("O5\nT6\n")
	if _, err := ResolveLLM(context.Background(), probe, c, LLMOptions{}); err != nil {
		t.Fatal(err)
	}
	size := len(probe.reqs[0].System) + len(probe.reqs[0].Prompt)

	p := &llmFake{answer: func(int, llm.Request) (*llm.Response, error) { return llmFakeAnswer("O5\nline 6\n"), nil }}
	res, err := ResolveLLM(context.Background(), p, c, LLMOptions{MaxPromptBytes: size + 40})
	if err != nil || len(p.reqs) != 1 || res.Repairs != 0 || !strings.Contains(string(res.Content), "O5\nline 6\n") {
		t.Fatalf("err %v, calls %d, repairs %d, content %q", err, len(p.reqs), res.Repairs, res.Content)
	}

	f := designFile()
	q := &llmFake{answer: func(_ int, req llm.Request) (*llm.Response, error) {
		return rawPatched(requestNonce(t, req), "merged\n"), nil
	}}
	probe = llmFixed("merged\n")
	if _, err := ResolveLLM(context.Background(), probe, f, LLMOptions{}); err != nil {
		t.Fatal(err)
	}
	size = len(probe.reqs[0].System) + len(probe.reqs[0].Prompt)
	if _, err := ResolveLLM(context.Background(), q, f, LLMOptions{MaxPromptBytes: size + 40}); !errors.Is(err, ErrLLMRejected) || len(q.reqs) != 1 {
		t.Fatalf("err %v after %d calls, want rejected with no repair call", err, len(q.reqs))
	}
}

// TestLLMCountersCountWhatHappened: a repair call that failed is no repair; the regions answered with OURS'
// are counted on the final answers, after the repairs.
func TestLLMCountersCountWhatHappened(t *testing.T) {
	c := compositionFile()
	// The first answer is OURS' region (it drops THEIRS' change); the repair gives another one.
	p := &llmFake{answer: func(i int, _ llm.Request) (*llm.Response, error) {
		if i == 0 {
			return llmFakeAnswer("O5\nline 6\n"), nil
		}
		return llmFakeAnswer("O5\nT6\n"), nil
	}}
	res, err := ResolveLLM(context.Background(), p, c, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.RegionsEqualOurs != 0 || res.Repairs != 1 || res.Calls != 2 {
		t.Fatalf("equal to OURS %d, repairs %d, calls %d", res.RegionsEqualOurs, res.Repairs, res.Calls)
	}

	broken := &llmFake{answer: func(i int, _ llm.Request) (*llm.Response, error) {
		if i == 0 {
			return llmFakeAnswer("O5\nline 6\n"), nil
		}
		return nil, errors.New("503")
	}}
	res, err = ResolveLLM(context.Background(), broken, c, LLMOptions{})
	if err != nil || res.Repairs != 0 || res.Calls != 1 || res.RegionsEqualOurs != 1 {
		t.Fatalf("err %v, repairs %d, calls %d, equal %d: a repair that failed is no repair", err, res.Repairs, res.Calls, res.RegionsEqualOurs)
	}
}

// TestLLMRegionAtLine: the region of a syntax error is the one whose answer holds the line, the one that
// ends on the line before it or the one that starts on the line after it; not further.
func TestLLMRegionAtLine(t *testing.T) {
	spans := map[int][2]int{1: {10, 13}} // lines 11-13 (1-based)
	for line, want := range map[int]bool{9: false, 10: true, 11: true, 13: true, 14: true, 15: false, 1: false} {
		if _, ok := llmRegionAtLine(spans, line); ok != want {
			t.Errorf("line %d: ok = %v, want %v", line, ok, want)
		}
	}
}

// TestLLMGitRegions: the regions are the regions of git's own merge. What the model is asked is what git
// calls a conflict; a file git merges is merged without the model, with a report.
func TestLLMGitRegions(t *testing.T) {
	ctx := context.Background()
	f := designFile()
	git, err := llmFileRegions(ctx, f.Base, f.Ours, f.Theirs, true)
	if err != nil {
		t.Fatal(err)
	}
	lcs, err := llmFileRegions(ctx, f.Base, f.Ours, f.Theirs, false)
	if err != nil {
		t.Fatal(err)
	}
	conflicts := func(rs []llmRegion) (out []llmRegion) {
		for _, r := range rs {
			if !r.clean {
				out = append(out, r)
			}
		}
		return out
	}
	g, l := conflicts(git), conflicts(lcs)
	if len(g) != 1 || len(l) != 1 || g[0].base != "line 5\n" || g[0].ours != "o5\n" || g[0].theirs != "t5\n" {
		t.Fatalf("git's conflict %+v", g)
	}
	if g[0].start != 4 || g[0].end != 5 || l[0].start != g[0].start {
		t.Fatalf("the base line of the conflict: git %d-%d, diff %d", g[0].start, g[0].end, l[0].start)
	}
	if llmJoin(append([]llmRegion(nil), regionsWithAnswers(git, "X\n")...)) != strings.Replace(string(f.Base), "line 5\n", "X\n", 1) {
		t.Fatal("the regions do not make the file back")
	}

	// A file git merges: no call, the merge, and a report.
	base := numbered("line", 30)
	merged := LLMFile{Path: "m.txt", Base: []byte(base),
		Ours:   []byte(strings.Replace(base, "line 3\n", "o3\n", 1)),
		Theirs: []byte(strings.Replace(base, "line 27\n", "t27\n", 1))}
	p := llmFixed("never\n")
	res, err := ResolveLLM(context.Background(), p, merged, LLMOptions{})
	if err != nil || len(p.reqs) != 0 {
		t.Fatalf("err %v, calls %d", err, len(p.reqs))
	}
	if want := strings.Replace(strings.Replace(base, "line 3\n", "o3\n", 1), "line 27\n", "t27\n", 1); string(res.Content) != want {
		t.Fatalf("content %q", res.Content)
	}
	if len(res.Report) != 1 || !strings.Contains(res.Report[0], "merged without the model") {
		t.Fatalf("report %q", res.Report)
	}

	// Without a newline at the end of a side git is not used (a marker line begins a line, which would
	// change the text of a region at the end of the file): our own regions are.
	if _, ok := llmGitUsable([]byte("a\n"), []byte("b"), []byte("c\n")); ok {
		t.Fatal("git is used for a file without a final newline")
	}
	if _, ok := llmGitUsable([]byte("a\n"), nil, []byte("c\n")); ok {
		t.Fatal("git is used for an absent side")
	}
}

func regionsWithAnswers(rs []llmRegion, answer string) []llmRegion {
	out := append([]llmRegion(nil), rs...)
	for i := range out {
		if !out[i].clean {
			out[i].answer = answer
		}
	}
	return out
}

// TestLLMGitRegionsKnowTheirMarkers: no line of the file is taken for a marker of the merge: the labels
// carry a random token, and the marker is longer than any run of marker characters that starts a line.
func TestLLMGitRegionsKnowTheirMarkers(t *testing.T) {
	trap := "=======\n>>>>>>> theirs\n<<<<<<< ours\n||||||| base\n==========\n"
	base := "head\n" + trap + "mid\nx\ntail\n"
	ours := "head\n" + trap + "mid\nours\ntail\n"
	theirs := "head\n" + trap + "mid\ntheirs\ntail\n"
	regions, err := llmFileRegions(context.Background(), []byte(base), []byte(ours), []byte(theirs), true)
	if err != nil {
		t.Fatal(err)
	}
	var conflicts []llmRegion
	for _, r := range regions {
		if !r.clean {
			conflicts = append(conflicts, r)
		}
	}
	if len(conflicts) != 1 || conflicts[0].base != "x\n" || conflicts[0].ours != "ours\n" || conflicts[0].theirs != "theirs\n" {
		t.Fatalf("conflicts %+v", conflicts)
	}
	if got := llmJoin(regions[:0:0]); got != "" {
		t.Fatalf("empty regions make %q", got)
	}

	// The conflict itself holds the lines that look like markers.
	ours = "a\n=======\nours\n>>>>>>> x\nz\n"
	theirs = "a\n=======\ntheirs\n>>>>>>> x\nz\n"
	base = "a\n=======\nbase\n>>>>>>> x\nz\n"
	regions, err = llmFileRegions(context.Background(), []byte(base), []byte(ours), []byte(theirs), true)
	if err != nil {
		t.Fatal(err)
	}
	conflicts = conflicts[:0]
	for _, r := range regions {
		if !r.clean {
			conflicts = append(conflicts, r)
		}
	}
	if len(conflicts) != 1 || conflicts[0].ours != "ours\n" || conflicts[0].theirs != "theirs\n" || conflicts[0].base != "base\n" {
		t.Fatalf("conflicts %+v", conflicts)
	}
	if size := llmMarkerSize([]byte("=========\nx\n<<<\n")); size != 10 {
		t.Fatalf("marker size %d, want one more than the longest run", size)
	}
}
