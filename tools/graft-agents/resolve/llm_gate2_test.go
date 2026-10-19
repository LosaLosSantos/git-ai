//go:build cgo

package resolve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

func TestResolveLLMDeclinesInvalidUTF8WithoutCall(t *testing.T) {
	// 0xE9 is Latin-1 "é": not valid UTF-8 on its own. Every side is checked,
	// including the base, and each case must decline before any provider call.
	for name, f := range map[string]LLMFile{
		"ours invalid":    {Path: "x", Base: []byte("caf\n"), Ours: []byte("caf\xe9\nB\n"), Theirs: []byte("caf\nC\n")},
		"theirs invalid":  {Path: "x", Base: []byte("caf\n"), Ours: []byte("caf\nB\n"), Theirs: []byte("caf\xe9\nC\n")},
		"base invalid":    {Path: "x", Base: []byte("caf\xe9\nA\n"), Ours: []byte("caf\nB\n"), Theirs: []byte("caf\nC\n")},
		"add-add invalid": {Path: "x", Ours: []byte("\xff\n"), Theirs: []byte("t\n")},
	} {
		t.Run(name, func(t *testing.T) {
			p := llmFixed("x\n")
			if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{}); !errors.Is(err, ErrLLMNotApplicable) {
				t.Fatalf("err = %v, want ErrLLMNotApplicable", err)
			}
			if len(p.reqs) != 0 {
				t.Fatalf("provider calls = %d, want none", len(p.reqs))
			}
		})
	}
}

func TestResolveLLMAcceptsValidMultibyteUTF8(t *testing.T) {
	// Valid multi-byte text ("é" as C3 A9) is still sent to the model.
	p := llmFixed("café\nB\nC\n")
	f := LLMFile{Path: "x", Base: []byte("café\nA\n"), Ours: []byte("café\nB\n"), Theirs: []byte("café\nC\n")}
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 1 {
		t.Fatalf("calls = %d, want 1", len(p.reqs))
	}
}

func TestLLMStrategyDeclinesInvalidUTF8WithoutCall(t *testing.T) {
	// Through the strategy chain: a Latin-1 side must not reach the provider,
	// and no strategy may write the altered bytes.
	fake := wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		return wp19Answer("caf\xe9\nB\nC\n"), nil
	})
	r := wp19Build(t, wp19LLMConfig)

	file := wp19Conflict("a/b.txt")
	file.Ours.Content = []byte("caf\xe9\nB\n")
	file.Theirs.Content = []byte("caf\xe9\nC\n")
	file.Base.Content = []byte("caf\xe9\nA\n")

	resp, err := r.Resolve(context.Background(), wp19Request(file))
	if err == nil {
		t.Fatalf("resolved a non-UTF-8 file: %+v", resp)
	}
	if fake.calls() != 0 {
		t.Fatalf("provider calls = %d, want none", fake.calls())
	}
}

// TestResolveLLMKeepsCRLFLineEndings: a file whose sides are all CRLF reaches the model as LF, a
// model that answers with LF, as models do, gets its answer accepted, and the result is CRLF
// again; an answer that echoes CRLF is read as LF and accepted too; the newline rule still holds.
func TestResolveLLMKeepsCRLFLineEndings(t *testing.T) {
	base := strings.ReplaceAll(numbered("line", 30), "\n", "\r\n")
	ours := strings.Replace(base, "line 15\r\n", "o\r\n", 1)
	theirs := strings.Replace(base, "line 15\r\n", "t\r\n", 1)
	f := LLMFile{Path: "crlf.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}
	want := strings.Replace(base, "line 15\r\n", "merged\r\n", 1)

	// A realistic model: whatever it reads, it answers with LF line breaks.
	p := &llmFake{answer: func(_ int, req llm.Request) (*llm.Response, error) {
		return llmRawAnswer(strings.ReplaceAll(patchWrap(t, req, "merged\n"), "\r", "")), nil
	}}
	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Content) != want {
		t.Fatalf("content = %q", res.Content)
	}
	if strings.Contains(p.reqs[0].System+p.reqs[0].Prompt, "\r") {
		t.Fatal("a CRLF file reached the model with CR")
	}

	// A model that echoes CRLF in the code: its answer is read as LF.
	echo := &llmFake{answer: func(_ int, req llm.Request) (*llm.Response, error) {
		n := patchNonceRE.FindStringSubmatch(req.Prompt)[1]
		body := "line 12\r\nline 13\r\nline 14\r\nmerged\r\nline 16\r\nline 17\r\nline 18\r\n"
		return llmRawAnswer("===== BEGIN PATCHED CODE " + n + " =====\n" + body + "===== END PATCHED CODE " + n + " =====\n"), nil
	}}
	res, err = ResolveLLM(context.Background(), echo, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Content) != want {
		t.Fatalf("content of a CRLF echo = %q", res.Content)
	}

	if _, err := ResolveLLM(context.Background(), llmFixed("merged"), f, LLMOptions{}); !errors.Is(err, ErrLLMRejected) {
		t.Fatalf("err = %v, want the newline rule", err)
	}
}

// TestResolveLLMLineEndingsBySidesPresent: an add/add conflict (no base) whose sides are CRLF is a
// CRLF file; a file with mixed line endings is sent as it is.
func TestResolveLLMLineEndingsBySidesPresent(t *testing.T) {
	ours := strings.ReplaceAll(numbered("line", 20), "\n", "\r\n")
	theirs := strings.Replace(ours, "line 10\r\n", "t\r\n", 1)
	ours = strings.Replace(ours, "line 10\r\n", "o\r\n", 1)
	p := llmFixed("merged\n")
	res, err := ResolveLLM(context.Background(), p, LLMFile{Path: "a.txt", Ours: []byte(ours), Theirs: []byte(theirs)}, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.reqs[0].Prompt, "\r") || !strings.Contains(string(res.Content), "merged\r\n") {
		t.Fatalf("add/add CRLF: prompt has CR %v, content %q", strings.Contains(p.reqs[0].Prompt, "\r"), res.Content)
	}

	mixed := strings.Replace(ours, "line 3\r\n", "line 3\n", 1)
	p = llmFixed("merged\n")
	_, _ = ResolveLLM(context.Background(), p, LLMFile{Path: "a.txt", Ours: []byte(mixed), Theirs: []byte(theirs)}, LLMOptions{})
	if len(p.reqs) == 0 || !strings.Contains(p.reqs[0].Prompt, "\r") {
		t.Fatal("a file with mixed line endings was not sent as it is")
	}

	for _, tc := range []struct {
		sides [][]byte
		want  string
	}{
		{[][]byte{[]byte("a\nb\n"), nil, []byte("c\n")}, LineEndingsLF},
		{[][]byte{[]byte("a\r\nb\r\n"), nil, []byte("c\r\nd")}, LineEndingsCRLF},
		{[][]byte{[]byte("a\r\nb\n"), []byte("c\r\n")}, LineEndingsMixed},
		{[][]byte{[]byte("a\r\n"), []byte("c\n")}, LineEndingsMixed},
		{[][]byte{[]byte("no break"), nil}, LineEndingsLF},
	} {
		if got := LLMLineEndings(tc.sides...); got != tc.want {
			t.Fatalf("%q: %s, want %s", tc.sides, got, tc.want)
		}
	}
}

// TestResolveLLMRecordsRejectedAnswers: the raw text of a rejected answer reaches the
// LLMRegionStats of the context, with why it was rejected.
func TestResolveLLMRecordsRejectedAnswers(t *testing.T) {
	stats := &LLMRegionStats{}
	f := LLMFile{Path: "a.txt", Base: []byte("base\n"), Ours: []byte("ours\n"), Theirs: []byte("theirs\n")}
	raw := &llmFake{answer: func(int, llm.Request) (*llm.Response, error) { return llmRawAnswer("no block at all"), nil }}
	_, err := ResolveLLM(WithLLMRegionStats(context.Background(), stats), raw, f, LLMOptions{})
	if !errors.Is(err, ErrLLMRejected) {
		t.Fatalf("err = %v", err)
	}
	got := stats.Rejected()
	if len(got) != 1 || !strings.Contains(got[0], "no block at all") || !strings.Contains(got[0], "has no patched code") {
		t.Fatalf("rejected = %q", got)
	}
}
