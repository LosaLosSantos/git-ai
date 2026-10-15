package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/open_scion/tools/graft-agents/llm"
)

// llmFake is a llm.Provider that records requests and answers with answer.
type llmFake struct {
	reqs   []llm.Request
	answer func(i int, req llm.Request) (*llm.Response, error)
}

func (f *llmFake) Name() string { return "fake:test" }

func (f *llmFake) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.reqs = append(f.reqs, req)
	return f.answer(len(f.reqs)-1, req)
}

func llmFakeAnswer(content, confidence string) *llm.Response {
	b, _ := json.Marshal(map[string]string{"content": content, "confidence": confidence, "explanation": "fake"})
	return &llm.Response{Text: string(b), JSON: b, Model: "fake-model"}
}

func llmFixed(content, confidence string) *llmFake {
	return &llmFake{answer: func(int, llm.Request) (*llm.Response, error) {
		return llmFakeAnswer(content, confidence), nil
	}}
}

func numbered(prefix string, n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "%s %d\n", prefix, i)
	}
	return sb.String()
}

func TestLLMDiff(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		want []llmEdit
	}{
		{name: "identical", a: "a\nb\n", b: "a\nb\n", want: nil},
		{name: "replace", a: "a\nb\nc\n", b: "a\nX\nc\n", want: []llmEdit{{start: 1, end: 2, repl: []string{"X\n"}}}},
		{name: "insert", a: "a\nc\n", b: "a\nb\nc\n", want: []llmEdit{{start: 1, end: 1, repl: []string{"b\n"}}}},
		{name: "delete", a: "a\nb\nc\n", b: "a\nc\n", want: []llmEdit{{start: 1, end: 2}}},
		{name: "add to empty", a: "", b: "x\n", want: []llmEdit{{start: 0, end: 0, repl: []string{"x\n"}}}},
		{name: "delete all", a: "x\n", b: "", want: []llmEdit{{start: 0, end: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := llmDiff(llmSplitLines([]byte(tc.a)), llmSplitLines([]byte(tc.b)), defaultLLMMaxDiffCells)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("edits = %v, want %v", got, tc.want)
			}
			// Applying the edits to a must reproduce b.
			a := llmSplitLines([]byte(tc.a))
			if out := llmApply(a, 0, len(a), editsAsHunks(got, llmSideOurs), llmSideOurs); out != tc.b {
				t.Fatalf("applied = %q, want %q", out, tc.b)
			}
		})
	}
}

func editsAsHunks(edits []llmEdit, side int) []llmHunk {
	out := make([]llmHunk, 0, len(edits))
	for _, e := range edits {
		out = append(out, llmHunk{llmEdit: e, side: side})
	}
	return out
}

func TestLLMDiffTooLarge(t *testing.T) {
	a := llmSplitLines([]byte(numbered("a", 100)))
	b := llmSplitLines([]byte(numbered("b", 100)))
	if _, err := llmDiff(a, b, 50); err == nil {
		t.Fatal("expected cell limit error")
	}
}

func TestResolveLLMNotApplicable(t *testing.T) {
	for name, f := range map[string]LLMFile{
		"ours absent":   {Path: "x", Base: []byte("a\n"), Theirs: []byte("b\n")},
		"theirs absent": {Path: "x", Base: []byte("a\n"), Ours: []byte("b\n")},
		"binary":        {Path: "x", Base: []byte("a\x00"), Ours: []byte("b\n"), Theirs: []byte("c\n")},
	} {
		t.Run(name, func(t *testing.T) {
			p := llmFixed("x", "high")
			if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{}); !errors.Is(err, ErrLLMNotApplicable) {
				t.Fatalf("err = %v", err)
			}
			if len(p.reqs) != 0 {
				t.Fatal("provider called for a non-applicable file")
			}
		})
	}
}

func TestResolveLLMWholeFile(t *testing.T) {
	p := llmFixed("merged\n", "high")
	f := LLMFile{Path: "a/b.go", Base: []byte("base\n"), Ours: []byte("ours\n"), Theirs: []byte("theirs\n")}

	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Content) != "merged\n" || res.Confidence != LLMConfidenceHigh || res.Model != "fake-model" || res.Hunks != 0 {
		t.Fatalf("result = %+v", res)
	}
	if res.Explanation != "fake" {
		t.Fatalf("explanation = %q", res.Explanation)
	}

	if len(p.reqs) != 1 {
		t.Fatalf("calls = %d", len(p.reqs))
	}
	req := p.reqs[0]
	for _, want := range []string{"a/b.go", "BEGIN BASE", "base\n", "BEGIN OURS", "ours\n", "BEGIN THEIRS", "theirs\n"} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt misses %q", want)
		}
	}
	if !strings.Contains(req.System, "complete merged file") {
		t.Error("system prompt does not ask for the complete file")
	}
	if req.SchemaName == "" || req.JSONSchema == nil {
		t.Fatal("schema not requested")
	}
	required, _ := json.Marshal(req.JSONSchema["required"])
	if string(required) != `["content","confidence","explanation"]` {
		t.Fatalf("schema required = %s", required)
	}
}

func TestResolveLLMAddAddBaseAbsent(t *testing.T) {
	p := llmFixed("new\n", "medium")
	f := LLMFile{Path: "new.txt", Ours: []byte("o\n"), Theirs: []byte("t\n")}
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.reqs[0].Prompt, "(absent)") {
		t.Fatal("absent base not marked in prompt")
	}
}

func TestResolveLLMRejections(t *testing.T) {
	f := LLMFile{Path: "x", Base: []byte("a\n"), Ours: []byte("b\n"), Theirs: []byte("c\n")}

	for name, tc := range map[string]struct {
		resp *llm.Response
		opt  LLMOptions
	}{
		"low confidence by default":          {resp: llmFakeAnswer("x\n", "low")},
		"medium rejected when high required": {resp: llmFakeAnswer("x\n", "medium"), opt: LLMOptions{MinConfidence: "high"}},
		"unknown confidence":                 {resp: llmFakeAnswer("x\n", "certain")},
		"conflict marker":                    {resp: llmFakeAnswer("<<<<<<< ours\nx\n", "high")},
		"separator marker":                   {resp: llmFakeAnswer("x\n=======\ny\n", "high")},
		"answer too large":                   {resp: llmFakeAnswer(strings.Repeat("x", 100), "high"), opt: LLMOptions{MaxAnswerBytes: 10}},
		"invalid JSON":                       {resp: &llm.Response{Text: "no json here"}},
		"wrong JSON type":                    {resp: &llm.Response{JSON: json.RawMessage(`{"content": 3}`)}},
	} {
		t.Run(name, func(t *testing.T) {
			p := &llmFake{answer: func(int, llm.Request) (*llm.Response, error) { return tc.resp, nil }}
			_, err := ResolveLLM(context.Background(), p, f, tc.opt)
			if !errors.Is(err, ErrLLMRejected) {
				t.Fatalf("err = %v, want rejection", err)
			}
		})
	}
}

func TestResolveLLMAcceptsSetextUnderline(t *testing.T) {
	// "==========" is a title underline, not the "=======" separator, and the
	// server accepts it; the answer check must not reject it.
	f := LLMFile{Path: "README.md", Base: []byte("Title\n==========\nbody\n"), Ours: []byte("Title\n==========\nours\n"), Theirs: []byte("Title\n==========\ntheirs\n")}
	want := "Title\n==========\nmerged\n"
	res, err := ResolveLLM(context.Background(), llmFixed(want, "high"), f, LLMOptions{})
	if err != nil {
		t.Fatalf("ResolveLLM: %v", err)
	}
	if string(res.Content) != want {
		t.Fatalf("content = %q, want %q", res.Content, want)
	}
	if llmHasConflictMarkers(want) {
		t.Fatal("setext underline treated as a conflict marker")
	}
}

func TestResolveLLMProviderError(t *testing.T) {
	boom := errors.New("boom")
	p := &llmFake{answer: func(int, llm.Request) (*llm.Response, error) { return nil, boom }}
	f := LLMFile{Path: "x", Base: []byte("a\n"), Ours: []byte("b\n"), Theirs: []byte("c\n")}

	_, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(err, ErrLLMRejected) || errors.Is(err, ErrLLMNotApplicable) {
		t.Fatal("provider failure must not look like a rejection")
	}
}

func TestResolveLLMInvalidOptions(t *testing.T) {
	f := LLMFile{Path: "x", Base: []byte("a\n"), Ours: []byte("b\n"), Theirs: []byte("c\n")}
	if _, err := ResolveLLM(context.Background(), llmFixed("x", "high"), f, LLMOptions{MinConfidence: "sure"}); err == nil {
		t.Fatal("invalid min confidence accepted")
	}
	if _, err := ResolveLLM(context.Background(), nil, f, LLMOptions{}); err == nil {
		t.Fatal("nil provider accepted")
	}
}

func TestResolveLLMHunkModeReassemblesRegions(t *testing.T) {
	base := numbered("line", 100)
	// Ours changes line 50 only. Theirs changes line 10 only. Both change line 80
	// differently, which is the conflict.
	ours := strings.Replace(base, "line 50\n", "ours 50\n", 1)
	ours = strings.Replace(ours, "line 80\n", "ours 80\n", 1)
	theirs := strings.Replace(base, "line 10\n", "theirs 10\n", 1)
	theirs = strings.Replace(theirs, "line 80\n", "theirs 80\n", 1)

	p := &llmFake{answer: func(int, llm.Request) (*llm.Response, error) {
		return llmFakeAnswer("merged 80\n", "high"), nil
	}}
	f := LLMFile{Path: "big.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}

	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 1500, ContextLines: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hunks != 1 || len(p.reqs) != 1 {
		t.Fatalf("hunks = %d, calls = %d; want one conflict answered once", res.Hunks, len(p.reqs))
	}

	want := strings.NewReplacer(
		"line 50\n", "ours 50\n",
		"line 10\n", "theirs 10\n",
		"line 80\n", "merged 80\n",
	).Replace(base)
	if string(res.Content) != want {
		t.Fatalf("content mismatch:\n%s", string(res.Content))
	}

	prompt := p.reqs[0].Prompt
	if !strings.Contains(prompt, "ours 80") || !strings.Contains(prompt, "theirs 80") {
		t.Error("prompt misses the conflicting sides")
	}
	if strings.Contains(prompt, "line 10\n") || strings.Contains(prompt, "line 50\n") {
		t.Error("prompt contains unrelated regions")
	}
	if !strings.Contains(prompt, "line 78\n") {
		t.Error("prompt misses the context before the region")
	}
	if strings.Contains(p.reqs[0].System, "complete merged file") {
		t.Error("hunk mode must not ask for the whole file")
	}
}

func TestResolveLLMHunkModeNoConflictSkipsModel(t *testing.T) {
	base := numbered("line", 100)
	ours := strings.Replace(base, "line 3\n", "ours 3\n", 1)
	theirs := strings.Replace(base, "line 40\n", "theirs 40\n", 1)

	p := llmFixed("unused\n", "high")
	f := LLMFile{Path: "big.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}

	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 0 || res.Hunks != 0 {
		t.Fatalf("calls = %d, hunks = %d; want none", len(p.reqs), res.Hunks)
	}
	want := strings.NewReplacer("line 3\n", "ours 3\n", "line 40\n", "theirs 40\n").Replace(base)
	if string(res.Content) != want {
		t.Fatal("non-overlapping changes not merged")
	}
}

func TestResolveLLMHunkModeTwoConflicts(t *testing.T) {
	base := numbered("line", 100)
	ours := strings.NewReplacer("line 5\n", "o5\n", "line 50\n", "o50\n").Replace(base)
	theirs := strings.NewReplacer("line 5\n", "t5\n", "line 50\n", "t50\n").Replace(base)

	p := &llmFake{answer: func(i int, _ llm.Request) (*llm.Response, error) {
		return llmFakeAnswer(fmt.Sprintf("resolved %d", i), map[int]string{0: "high", 1: "medium"}[i]), nil
	}}
	f := LLMFile{Path: "two.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}

	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 1500, ContextLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 2 || res.Hunks != 2 {
		t.Fatalf("calls = %d, hunks = %d", len(p.reqs), res.Hunks)
	}
	if res.Confidence != LLMConfidenceMedium {
		t.Fatalf("confidence = %s, want the lowest of the answers", res.Confidence)
	}
	want := strings.NewReplacer(
		"line 5\n", "resolved 0\n",
		"line 50\n", "resolved 1\n",
	).Replace(base)
	if string(res.Content) != want {
		t.Fatalf("content mismatch:\n%s", string(res.Content))
	}
	if !strings.Contains(p.reqs[0].Prompt, "region 1 of 2") {
		t.Fatal("prompt does not number the region")
	}
}

func TestResolveLLMHunkModeRejectsBadRegionAnswer(t *testing.T) {
	base := numbered("line", 100)
	ours := strings.Replace(base, "line 15\n", "o\n", 1)
	theirs := strings.Replace(base, "line 15\n", "t\n", 1)

	p := llmFixed("<<<<<<< ours\n", "high")
	f := LLMFile{Path: "x", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 1500}); !errors.Is(err, ErrLLMRejected) {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveLLMHunkModeAddsMissingNewline(t *testing.T) {
	base := numbered("line", 100)
	ours := strings.Replace(base, "line 15\n", "o\n", 1)
	theirs := strings.Replace(base, "line 15\n", "t\n", 1)

	p := llmFixed("resolved", "high")
	f := LLMFile{Path: "x", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}
	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Content), "resolved\nline 16\n") {
		t.Fatalf("answer not newline-terminated:\n%s", res.Content)
	}
}

func TestLLMRegionsCleanWhenSidesAgree(t *testing.T) {
	base := llmSplitLines([]byte("a\nb\nc\n"))
	ours := llmSplitLines([]byte("a\nX\nc\n"))
	theirs := llmSplitLines([]byte("a\nX\nc\n"))
	oe, _ := llmDiff(base, ours, defaultLLMMaxDiffCells)
	te, _ := llmDiff(base, theirs, defaultLLMMaxDiffCells)

	regions := llmRegions(base, oe, te)
	if len(regions) != 1 || !regions[0].clean {
		t.Fatalf("regions = %+v, want one clean region", regions)
	}
	if got := llmAssemble(base, regions); got != "a\nX\nc\n" {
		t.Fatalf("assembled = %q", got)
	}
}

func TestLLMEmptyFileSides(t *testing.T) {
	p := llmFixed("", "high")
	f := LLMFile{Path: "empty.txt", Base: []byte("x\n"), Ours: []byte{}, Theirs: []byte{}}
	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 0 {
		t.Fatalf("content = %q", res.Content)
	}
}
