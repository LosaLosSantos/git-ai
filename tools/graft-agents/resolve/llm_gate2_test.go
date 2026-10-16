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
			p := llmFixed("x\n", "high")
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
	p := llmFixed("café\nB\nC\n", "high")
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
		return wp19Answer("caf\xe9\nB\nC\n", "high"), nil
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

func TestLLMEnsureNewlineFollowsLineEndings(t *testing.T) {
	crlfBase := []string{"a\r\n", "b\r\n", "c\r\n"}
	lfBase := []string{"a\n", "b\n", "c\n"}

	// A CRLF neighbourhood gets CRLF, an LF file gets LF.
	crlf := &llmRegion{start: 1, end: 2, base: "b\r\n", ours: "B\r\n", theirs: "b2\r\n"}
	if got := llmEnsureNewline("X", crlfBase, crlf); got != "X\r\n" {
		t.Fatalf("CRLF answer = %q, want %q", got, "X\r\n")
	}
	lf := &llmRegion{start: 1, end: 2, base: "b\n", ours: "B\n", theirs: "b2\n"}
	if got := llmEnsureNewline("X", lfBase, lf); got != "X\n" {
		t.Fatalf("LF answer = %q, want %q", got, "X\n")
	}

	// An answer that already ends in a newline is left alone.
	if got := llmEnsureNewline("X\r\n", crlfBase, crlf); got != "X\r\n" {
		t.Fatalf("terminated answer = %q", got)
	}

	// The last line without a terminator gets none.
	last := &llmRegion{start: 2, end: 3, base: "c", ours: "C", theirs: "c2"}
	if got := llmEnsureNewline("X", []string{"a\r\n", "b\r\n", "c"}, last); got != "X" {
		t.Fatalf("unterminated last line = %q, want %q", got, "X")
	}
}

func TestResolveLLMHunkModeKeepsCRLFLineEndings(t *testing.T) {
	base := strings.ReplaceAll(numbered("line", 100), "\n", "\r\n")
	ours := strings.Replace(base, "line 15\r\n", "o\r\n", 1)
	theirs := strings.Replace(base, "line 15\r\n", "t\r\n", 1)
	f := LLMFile{Path: "crlf.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}

	// The answer has no line terminator; the region is not the last line.
	p := llmFixed("merged", "high")
	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hunks != 1 {
		t.Fatalf("hunks = %d, want 1", res.Hunks)
	}
	content := string(res.Content)
	if !strings.Contains(content, "merged\r\n") {
		t.Fatalf("answer not CRLF-terminated: %q", content)
	}
	if lf, crlf := strings.Count(content, "\n"), strings.Count(content, "\r\n"); lf != crlf {
		t.Fatalf("mixed line endings: %d LF, %d CRLF", lf, crlf)
	}
}

func TestLLMHunkPromptMarksAbsentBase(t *testing.T) {
	base := []string{"x\n"}
	r := &llmRegion{start: 0, end: 0, base: "", ours: "o\n", theirs: "t\n"}

	absent := llmHunkPrompt("f.txt", base, r, 1, 1, 8, true, "nonce1")
	if !strings.Contains(absent, "BEGIN BASE REGION nonce1 (absent) =====") {
		t.Fatalf("absent base not marked:\n%s", absent)
	}

	present := llmHunkPrompt("f.txt", base, r, 1, 1, 8, false, "nonce1")
	if strings.Contains(present, "(absent)") {
		t.Fatalf("present base marked absent:\n%s", present)
	}
}
