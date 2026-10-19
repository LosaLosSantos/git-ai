//go:build cgo

package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

func syntaxFile() LLMFile {
	return LLMFile{Path: "a.go",
		Base:   []byte("package a\n\nfunc f() int {\n\treturn 1\n}\n"),
		Ours:   []byte("package a\n\nfunc f() int {\n\treturn 2\n}\n"),
		Theirs: []byte("package a\n\nfunc f() int {\n\treturn 3\n}\n")}
}

// TestLLMDesignRepairOnTheSyntaxOfTheFile: the first syntax error of the assembled file is mapped to the
// region it is in and quoted back once, with the line it reads; the repaired answer is kept when the file
// parses. If the file still does not parse, the first answer is the result (the chain's own syntax check
// then rejects it with its own words) and the repair round was made once.
func TestLLMDesignRepairOnTheSyntaxOfTheFile(t *testing.T) {
	p := &llmFake{answer: func(i int, _ llm.Request) (*llm.Response, error) {
		if i == 0 {
			return llmFakeAnswer("\treturn 2 +\n"), nil
		}
		return llmFakeAnswer("\treturn 2 + 3\n"), nil
	}}
	res, err := ResolveLLM(context.Background(), p, syntaxFile(), LLMOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 2 || res.Repairs != 1 {
		t.Fatalf("requests %d, repairs %d", len(p.reqs), res.Repairs)
	}
	second := p.reqs[1].Prompt
	if !strings.Contains(second, "- syntax check: a.go does not parse as go") {
		t.Fatalf("the syntax finding is not quoted:\n%s", second)
	}
	// The previous answer is the block the model wrote, with the context lines of the CODE, as in every repair.
	nonce := requestNonce(t, p.reqs[1])
	if got := patchSection(t, second, "YOUR PREVIOUS ANSWER", nonce); !strings.Contains(got, "BEGIN PATCHED CODE "+nonce+" =====\npackage a\n\nfunc f() int {\n\treturn 2 +\n}\n===== END") {
		t.Fatalf("previous answer %q", got)
	}
	if !strings.Contains(string(res.Content), "return 2 + 3") {
		t.Fatalf("content %q", res.Content)
	}

	// Still broken after the round: the first answer stays, and one round was made.
	still := &llmFake{answer: func(int, llm.Request) (*llm.Response, error) { return llmFakeAnswer("\treturn 2 +\n"), nil }}
	res, err = ResolveLLM(context.Background(), still, syntaxFile(), LLMOptions{})
	if err != nil || len(still.reqs) != 2 || !strings.Contains(string(res.Content), "return 2 +\n") {
		t.Fatalf("err %v, requests %d, content %q", err, len(still.reqs), res.Content)
	}

	// Without the option: one call, and the answer is left to the chain's check.
	none := &llmFake{answer: func(int, llm.Request) (*llm.Response, error) { return llmFakeAnswer("\treturn 2 +\n"), nil }}
	if _, err := ResolveLLM(context.Background(), none, syntaxFile(), LLMOptions{NoRepair: true}); err != nil || len(none.reqs) != 1 {
		t.Fatalf("err %v after %d calls", err, len(none.reqs))
	}
}
