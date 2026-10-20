package resolve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// llmFake is a llm.Provider that records requests and answers with answer. The text of an answer
// is the replacement of the region, which Complete wraps as a well-behaved model would (patchWrap);
// an llmRawAnswer is sent as it is.
type llmFake struct {
	reqs   []llm.Request
	answer func(i int, req llm.Request) (*llm.Response, error)
}

func (f *llmFake) Name() string { return "fake:test" }

func (f *llmFake) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.reqs = append(f.reqs, req)
	resp, err := f.answer(len(f.reqs)-1, req)
	return patchWrapResponse(req, resp, err)
}

// patchWrapResponse wraps the text of resp with patchWrap, unless it is raw or an error.
func patchWrapResponse(req llm.Request, resp *llm.Response, err error) (*llm.Response, error) {
	if err != nil || resp == nil || resp.Model == llmRawModel {
		return resp, err
	}
	out := *resp
	out.Text = patchWrap(nil, req, resp.Text)
	return &out, nil
}

// llmFakeAnswer is an answer that replaces the region with content.
func llmFakeAnswer(content string) *llm.Response {
	return &llm.Response{Text: content, Model: "fake-model"}
}

func llmFixed(content string) *llmFake {
	return &llmFake{answer: func(int, llm.Request) (*llm.Response, error) {
		return llmFakeAnswer(content), nil
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
			p := llmFixed("x")
			if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{}); !errors.Is(err, ErrLLMNotApplicable) {
				t.Fatalf("err = %v", err)
			}
			if len(p.reqs) != 0 {
				t.Fatal("provider called for a non-applicable file")
			}
		})
	}
}

// TestResolveLLMSmallFile: a file whose only line conflicts is one region with no context.
func TestResolveLLMSmallFile(t *testing.T) {
	p := llmFixed("merged\n")
	f := LLMFile{Path: "a/b.go", Base: []byte("base\n"), Ours: []byte("ours\n"), Theirs: []byte("theirs\n")}

	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Content) != "merged\n" || res.Model != "fake-model" || res.Hunks != 1 || len(p.reqs) != 1 {
		t.Fatalf("result = %+v, calls %d", res, len(p.reqs))
	}
	req := p.reqs[0]
	nonce := patchNonceRE.FindStringSubmatch(req.Prompt)[1]
	if got := patchSection(t, req.Prompt, "CODE", nonce); got != "ours\n" {
		t.Fatalf("CODE = %q", got)
	}
	if got := patchSection(t, req.Prompt, "PATCH", nonce); got != "@@ -1 +1 @@\n-base\n+theirs\n" {
		t.Fatalf("PATCH = %q", got)
	}
	if strings.Contains(req.System, "starts with the first") || strings.Contains(req.System, "ends with the last") {
		t.Fatal("no context lines, so no anchor directive")
	}
}

// TestResolveLLMAddAddBaseAbsent: without a base the PATCH adds THEIRS' lines.
func TestResolveLLMAddAddBaseAbsent(t *testing.T) {
	p := llmFixed("new\n")
	f := LLMFile{Path: "new.txt", Ours: []byte("o\n"), Theirs: []byte("t\n")}
	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Content) != "new\n" {
		t.Fatalf("content = %q", res.Content)
	}
	nonce := patchNonceRE.FindStringSubmatch(p.reqs[0].Prompt)[1]
	if got := patchSection(t, p.reqs[0].Prompt, "PATCH", nonce); got != "@@ -0,0 +1 @@\n+t\n" {
		t.Fatalf("PATCH = %q", got)
	}
}

func TestResolveLLMRejections(t *testing.T) {
	f := LLMFile{Path: "x", Base: []byte("a\n"), Ours: []byte("b\n"), Theirs: []byte("c\n")}

	for name, tc := range map[string]struct {
		resp *llm.Response
		opt  LLMOptions
	}{
		"conflict marker":  {resp: llmFakeAnswer("<<<<<<< ours\nx\n")},
		"separator marker": {resp: llmFakeAnswer("x\n=======\ny\n")},
		"answer too large": {resp: llmFakeAnswer(strings.Repeat("x", 100) + "\n"), opt: LLMOptions{MaxAnswerBytes: 10}},
		"no patched code":  {resp: llmRawAnswer("no block here")},
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
	res, err := ResolveLLM(context.Background(), llmFixed("merged\n"), f, LLMOptions{})
	if err != nil {
		t.Fatalf("ResolveLLM: %v", err)
	}
	if want := "Title\n==========\nmerged\n"; string(res.Content) != want {
		t.Fatalf("content = %q, want %q", res.Content, want)
	}
	if llmHasConflictMarkers("Title\n==========\n") {
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
	if _, err := ResolveLLM(context.Background(), llmFixed("x\n"), f, LLMOptions{MaxTokens: -1}); err == nil {
		t.Fatal("negative max tokens accepted")
	}
	if _, err := ResolveLLM(context.Background(), nil, f, LLMOptions{}); err == nil {
		t.Fatal("nil provider accepted")
	}
}

// TestResolveLLMMergesCleanRegionsLocally: the changes of one side only are merged without the
// model; the model answers the one conflicting region, and its request holds no other region.
func TestResolveLLMMergesCleanRegionsLocally(t *testing.T) {
	base := numbered("line", 100)
	ours := strings.NewReplacer("line 50\n", "ours 50\n", "line 80\n", "ours 80\n").Replace(base)
	theirs := strings.NewReplacer("line 10\n", "theirs 10\n", "line 80\n", "theirs 80\n").Replace(base)
	p := llmFixed("merged 80\n")
	f := LLMFile{Path: "big.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}

	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hunks != 1 || len(p.reqs) != 1 {
		t.Fatalf("hunks = %d, calls = %d; want one conflict answered once", res.Hunks, len(p.reqs))
	}
	want := strings.NewReplacer("line 50\n", "ours 50\n", "line 10\n", "theirs 10\n", "line 80\n", "merged 80\n").Replace(base)
	if string(res.Content) != want {
		t.Fatalf("content mismatch:\n%s", res.Content)
	}
	req := p.reqs[0]
	if strings.Contains(req.Prompt, "line 10\n") || strings.Contains(req.Prompt, "line 50\n") || strings.Contains(req.System, "OTHER CODE") {
		t.Error("request holds unrelated regions")
	}
}

func TestResolveLLMNoConflictSkipsModel(t *testing.T) {
	base := numbered("line", 100)
	ours := strings.Replace(base, "line 3\n", "ours 3\n", 1)
	theirs := strings.Replace(base, "line 40\n", "theirs 40\n", 1)

	p := llmFixed("unused\n")
	f := LLMFile{Path: "big.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}

	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
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
	if got := llmJoin(regions); got != "X\n" {
		t.Fatalf("assembled = %q", got)
	}
}

func TestLLMEmptyFileSides(t *testing.T) {
	// Both sides emptied the file the same way: a clean region, no call.
	p := llmFixed("")
	f := LLMFile{Path: "empty.txt", Base: []byte("x\n"), Ours: []byte{}, Theirs: []byte{}}
	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 0 || len(p.reqs) != 0 {
		t.Fatalf("content = %q, calls %d", res.Content, len(p.reqs))
	}
}
