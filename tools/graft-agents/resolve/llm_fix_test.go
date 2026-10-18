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

// llmTextOnly answers with Text only, as the json_object and plain-text
// fallbacks of the providers do: the schema is not enforced on that path.
func llmTextOnly(text string) *llmFake {
	return &llmFake{answer: func(int, llm.Request) (*llm.Response, error) {
		return &llm.Response{Text: text, Model: "weak-model"}, nil
	}}
}

func TestLLMAnswerRequiresSchemaFields(t *testing.T) {
	base := []byte("base\n")
	ours := []byte("ours\n")
	theirs := []byte("theirs\n")

	for name, text := range map[string]string{
		"missing content":     `{"explanation":"done"}`,
		"missing explanation": `{"content":"merged\n"}`,
		"merged instead":      `{"merged":"base\nours\ntheirs\n","explanation":"done"}`,
		"null answer":         `null`,
		"empty object":        `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := llmTextOnly(text)
			_, err := ResolveLLM(context.Background(), p, LLMFile{Path: "a.txt", Base: base, Ours: ours, Theirs: theirs}, LLMOptions{})
			if !errors.Is(err, ErrLLMRejected) {
				t.Fatalf("whole file: err = %v, want ErrLLMRejected", err)
			}
			if name == "missing content" && !strings.Contains(err.Error(), "content") {
				t.Fatalf("error does not name the missing field: %v", err)
			}
		})
	}
}

func TestLLMAnswerRequiresSchemaFieldsInHunkMode(t *testing.T) {
	base := numbered("line", 100)
	ours := strings.Replace(base, "line 15\n", "ours 15\n", 1)
	theirs := strings.Replace(base, "line 15\n", "theirs 15\n", 1)

	// The region answer omits content: the region must not be deleted.
	p := llmTextOnly(`{"explanation":"done"}`)
	f := LLMFile{Path: "big.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}
	_, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 1500})
	if !errors.Is(err, ErrLLMRejected) {
		t.Fatalf("hunk mode: err = %v, want ErrLLMRejected", err)
	}
}

func TestLLMAnswerEmptyContentRejectedWhenSidesNonEmpty(t *testing.T) {
	// One reply must not truncate a file that both sides kept.
	p := llmTextOnly(`{"content":"","explanation":"both removed it"}`)
	f := LLMFile{Path: "a.txt", Base: []byte("x\n"), Ours: []byte("y\n"), Theirs: []byte("z\n")}
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{}); !errors.Is(err, ErrLLMRejected) {
		t.Fatalf("err = %v, want ErrLLMRejected", err)
	}
}

func TestLLMAnswerEmptyContentAcceptedWhenASideIsEmpty(t *testing.T) {
	// An empty side makes an empty answer plausible (the other side deleted it).
	p := llmTextOnly(`{"content":"","explanation":"deleted"}`)
	f := LLMFile{Path: "a.txt", Base: []byte("x\n"), Ours: []byte{}, Theirs: []byte("z\n")}
	res, err := ResolveLLM(context.Background(), p, f, LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestLLMStrategyRejectsAnswerWithoutContent(t *testing.T) {
	// The repro of the review: the answer is the wrong shape, so the land must
	// not write an empty file.
	fake := wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		return &llm.Response{Text: `{"merged":"base\nours\ntheirs\n","explanation":"done"}`}, nil
	})
	r := wp19Build(t, wp19LLMConfig)

	resp, err := r.Resolve(context.Background(), wp19Request(wp19Conflict("a/b.txt")))
	if err == nil {
		t.Fatalf("resolved an answer without content: %+v", resp)
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

func TestResolveLLMMaxTokensInHunkMode(t *testing.T) {
	base := numbered("line", 100)
	ours := strings.Replace(base, "line 15\n", "o\n", 1)
	theirs := strings.Replace(base, "line 15\n", "t\n", 1)
	p := llmFixed("merged\n")
	f := LLMFile{Path: "big.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}

	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 1500, MaxTokens: 9000}); err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 1 {
		t.Fatalf("calls = %d, want 1", len(p.reqs))
	}
	if got := p.reqs[0].MaxTokens; got != 9000 {
		t.Fatalf("max tokens = %d, want 9000", got)
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

func TestResolveLLMHunkModeCapsRegions(t *testing.T) {
	base := numbered("line", 100)
	ours := llmAlternatingEdits(base, "ours")
	theirs := llmAlternatingEdits(base, "theirs")
	f := LLMFile{Path: "many.txt", Base: []byte(base), Ours: []byte(ours), Theirs: []byte(theirs)}

	// 50 conflicting regions: above the default cap of 32, so declined before
	// any model call.
	p := llmFixed("merged\n")
	_, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 1500})
	if !errors.Is(err, ErrLLMNotApplicable) {
		t.Fatalf("err = %v, want ErrLLMNotApplicable", err)
	}
	if len(p.reqs) != 0 {
		t.Fatalf("calls = %d, want none for a declined file", len(p.reqs))
	}

	// An explicit cap that admits the file answers each region once.
	p = llmFixed("merged\n")
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{MaxPromptBytes: 1500, MaxRegions: 50}); err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 50 {
		t.Fatalf("calls = %d, want 50", len(p.reqs))
	}
}

func TestResolveLLMHunkModeBoundsRegionPrompt(t *testing.T) {
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
		t.Fatalf("calls = %d, want none: the region prompt exceeds the bound", len(p.reqs))
	}
}

// llmSectionBody returns the data lines of one prompt section.
func llmSectionBody(t *testing.T, prompt, title string) string {
	t.Helper()
	begin := "===== BEGIN " + title + " "
	i := strings.Index(prompt, begin)
	if i < 0 {
		t.Fatalf("prompt has no %s section:\n%s", title, prompt)
	}
	start := i + strings.IndexByte(prompt[i:], '\n') + 1
	end := strings.Index(prompt[start:], "===== END "+title+" ")
	if end < 0 {
		t.Fatalf("section %s is not closed:\n%s", title, prompt)
	}
	return prompt[start : start+end]
}

func TestLLMPromptSectionsAreExact(t *testing.T) {
	p := llmFixed("merged\n")
	f := LLMFile{Path: "a.txt", Base: []byte("a\nb"), Ours: []byte{}, Theirs: []byte("x\n")}
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{}); err != nil {
		t.Fatal(err)
	}
	prompt := p.reqs[0].Prompt

	// Data is verbatim. A side without a final newline gets the git marker,
	// so the model cannot mistake a missing newline for content.
	if got, want := llmSectionBody(t, prompt, "BASE"), "a\nb\n\\ No newline at end of data\n"; got != want {
		t.Fatalf("BASE section = %q, want %q", got, want)
	}
	// An empty side is an empty section, not a placeholder that a file could
	// spell out itself.
	if got := llmSectionBody(t, prompt, "OURS"); got != "" {
		t.Fatalf("OURS section = %q, want empty", got)
	}
	if got, want := llmSectionBody(t, prompt, "THEIRS"), "x\n"; got != want {
		t.Fatalf("THEIRS section = %q, want %q", got, want)
	}
}

func TestLLMPromptMarksAbsentBase(t *testing.T) {
	p := llmFixed("merged\n")
	f := LLMFile{Path: "new.txt", Ours: []byte("(empty)"), Theirs: []byte("t\n")}
	if _, err := ResolveLLM(context.Background(), p, f, LLMOptions{}); err != nil {
		t.Fatal(err)
	}
	prompt := p.reqs[0].Prompt
	if !strings.Contains(prompt, "(absent)") {
		t.Fatalf("absent base not marked:\n%s", prompt)
	}
	// A file whose content is literally "(empty)" keeps its content.
	if got, want := llmSectionBody(t, prompt, "OURS"), "(empty)\n\\ No newline at end of data\n"; got != want {
		t.Fatalf("OURS section = %q", got)
	}
}
