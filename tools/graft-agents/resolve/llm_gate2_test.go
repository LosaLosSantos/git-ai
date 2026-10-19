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

// TestResolveLLMKeepsCRLFLineEndings: the context of a CRLF file is CRLF, an answer that keeps
// CRLF lands as it is, and one whose region is not newline-terminated is refused rather than
// patched up (the newline rule).
func TestResolveLLMKeepsCRLFLineEndings(t *testing.T) {
	base := strings.ReplaceAll(numbered("line", 30), "\n", "\r\n")
	ours := strings.Replace(base, "line 15\r\n", "o\r\n", 1)
	theirs := strings.Replace(base, "line 15\r\n", "t\r\n", 1)
	f := LLMFile{Path: "crlf.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}

	res, err := ResolveLLM(context.Background(), llmFixed("merged\r\n"), f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	content := string(res.Content)
	if want := strings.Replace(base, "line 15\r\n", "merged\r\n", 1); content != want {
		t.Fatalf("content = %q", content)
	}

	if _, err := ResolveLLM(context.Background(), llmFixed("merged"), f, LLMOptions{}); !errors.Is(err, ErrLLMRejected) {
		t.Fatalf("err = %v, want the newline rule", err)
	}
}
