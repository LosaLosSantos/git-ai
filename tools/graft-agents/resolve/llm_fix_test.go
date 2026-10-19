//go:build cgo

package resolve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

func TestLLMStrategyRejectsAnswerWithoutPatchedCode(t *testing.T) {
	// An answer of the wrong shape must not write anything.
	fake := wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		return llmRawAnswer(`{"merged":"base\nours\ntheirs\n"}`), nil
	})
	r := wp19Build(t, strings.Replace(wp19LLMConfig, `paths = ["*.txt", "**/*.txt"]`, "paths = [\"*.txt\", \"**/*.txt\"]\nrepair = false", 1))

	resp, err := r.Resolve(context.Background(), wp19Request(wp19Conflict("a/b.txt")))
	if err == nil {
		t.Fatalf("resolved an answer without patched code: %+v", resp)
	}
	if fake.calls() != 1 {
		t.Fatalf("provider calls = %d", fake.calls())
	}
}

func TestLLMMaxTokensIsSentToProvider(t *testing.T) {
	fake := wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		return wp19Answer("merged\n"), nil
	})
	config := strings.Replace(wp19LLMConfig, "max_tokens = 512", "max_tokens = 64000", 1)
	r := wp19Build(t, config)

	if _, err := r.Resolve(context.Background(), wp19Request(wp19Conflict("a/b.txt"))); err != nil {
		t.Fatal(err)
	}
	if got := fake.lastReq().MaxTokens; got != 64000 {
		t.Fatalf("request max tokens = %d, want the configured 64000", got)
	}
}

func TestLLMMaxTokensUnsetLeavesProviderDefault(t *testing.T) {
	fake := wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		return wp19Answer("merged\n"), nil
	})
	config := strings.Replace(wp19LLMConfig, "max_tokens = 512\n", "", 1)
	r := wp19Build(t, config)

	if _, err := r.Resolve(context.Background(), wp19Request(wp19Conflict("a/b.txt"))); err != nil {
		t.Fatal(err)
	}
	if got := fake.lastReq().MaxTokens; got != 0 {
		t.Fatalf("request max tokens = %d, want 0 so that the provider default applies", got)
	}
}

func TestResolveLLMMaxTokensOnEveryCall(t *testing.T) {
	base := numbered("line", 100)
	ours := strings.NewReplacer("line 15\n", "o\n", "line 60\n", "o\n").Replace(base)
	theirs := strings.NewReplacer("line 15\n", "t\n", "line 60\n", "t\n").Replace(base)
	p := llmFixed("merged\n")
	f := LLMFile{Path: "big.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}

	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxTokens: 9000}); err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 2 {
		t.Fatalf("calls = %d, want 2", len(p.reqs))
	}
	for i, req := range p.reqs {
		if req.MaxTokens != 9000 {
			t.Fatalf("call %d: max tokens = %d, want 9000", i, req.MaxTokens)
		}
	}
}

// llmAlternatingEdits changes every odd line of base on one side. Two sides
// with different prefixes conflict on each of those lines.
func llmAlternatingEdits(base, prefix string) string {
	var pairs []string
	for i := 1; i <= 100; i += 2 {
		pairs = append(pairs, fmt.Sprintf("line %d\n", i), fmt.Sprintf("%s %d\n", prefix, i))
	}
	return strings.NewReplacer(pairs...).Replace(base)
}

// TestResolveLLMCapsRegions: every odd line changes on both sides, differently, so the file has 50
// conflicting regions, one per changed line.
func TestResolveLLMCapsRegions(t *testing.T) {
	base := numbered("line", 100)
	ours := llmAlternatingEdits(base, "ours")
	theirs := llmAlternatingEdits(base, "theirs")
	f := LLMFile{Path: "many.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}

	// 50 conflicting regions: over the default cap of 20, so declined before any call.
	p := llmFixed("merged\n")
	_, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if !errors.Is(err, ErrLLMNotApplicable) {
		t.Fatalf("err = %v, want ErrLLMNotApplicable", err)
	}
	if len(p.reqs) != 0 {
		t.Fatalf("calls = %d, want none for a declined file", len(p.reqs))
	}

	// A cap that admits the file answers each region once.
	p = llmFixed("merged\n")
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxRegions: 50}); err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 50 {
		t.Fatalf("calls = %d, want 50", len(p.reqs))
	}
}

func TestResolveLLMBoundsRegionRequest(t *testing.T) {
	// An add-add conflict: the single region is the whole file on both sides.
	ours := strings.Repeat("ours line with some padding text here\n", 800)
	theirs := strings.Repeat("theirs line with some padding text too\n", 800)
	f := LLMFile{Path: "new.txt", Ours: []byte(ours), Theirs: []byte(theirs)}

	p := llmFixed("merged\n")
	_, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 1024})
	if !errors.Is(err, ErrLLMNotApplicable) {
		t.Fatalf("err = %v, want ErrLLMNotApplicable", err)
	}
	if len(p.reqs) != 0 {
		t.Fatalf("calls = %d, want none: the region request exceeds the bound", len(p.reqs))
	}
}

// TestLLMRequestDataIsExact: data is verbatim, and a last line without a newline is followed by
// the data note, so the model cannot mistake a missing newline for content; an answer that keeps
// the line unterminated at the end of the file keeps it so.
func TestLLMRequestDataIsExact(t *testing.T) {
	p := llmFixed("e")
	f := LLMFile{Path: "a.txt", Base: []byte("a\nb"), Ours: []byte("a\nc"), Theirs: []byte("a\nd")}
	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Content) != "a\ne" {
		t.Fatalf("content = %q", res.Content)
	}
	prompt := p.reqs[0].Prompt
	nonce := patchNonceRE.FindStringSubmatch(prompt)[1]
	begin := "===== BEGIN CODE " + nonce + " =====\n"
	i := strings.Index(prompt, begin)
	j := strings.Index(prompt, "===== END CODE "+nonce)
	if got, want := prompt[i+len(begin):j], "a\nc\n\\ No newline at end of data\n"; got != want {
		t.Fatalf("CODE section = %q, want %q", got, want)
	}
	if got := patchSection(t, prompt, "PATCH", nonce); got != "@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+d\n\\ No newline at end of file\n" {
		t.Fatalf("PATCH = %q", got)
	}
}
