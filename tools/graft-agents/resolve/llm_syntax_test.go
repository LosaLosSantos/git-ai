//go:build cgo

package resolve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

const wp3Config = `
[[strategy]]
type = "llm"

[strategy.llm]
type = "wp19-resolve-fake"
`

func wp3Conflict(path string) protocol.FileConflict {
	return protocol.FileConflict{
		Path:   path,
		Kind:   protocol.KindContent,
		Base:   wp19Side(protocol.ModeFile, "package a\n\nvar x = 1\n"),
		Ours:   wp19Side(protocol.ModeFile, "package a\n\nvar x = 2\n"),
		Theirs: wp19Side(protocol.ModeFile, "package a\n\nvar x = 3\n"),
	}
}

func TestLLMAnswerMustParse(t *testing.T) {
	wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		return wp19Answer("package a\n\nvar x = 2 +\n"), nil
	})
	r := wp19Build(t, wp3Config)
	_, err := r.Resolve(context.Background(), wp19Request(wp3Conflict("a.go")))
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) || len(unresolved.Failures) != 1 {
		t.Fatalf("err = %v, want the file unresolved", err)
	}
	attempts := unresolved.Failures[0].Attempts
	if len(attempts) != 1 || attempts[0].Kind != AttemptCheckFailed {
		t.Fatalf("attempts %+v, want one check failure", attempts)
	}
	if msg := attempts[0].Err.Error(); !strings.Contains(msg, "a.go does not parse as go") || !strings.Contains(msg, "line ") {
		t.Fatalf("reason %q", msg)
	}
}

func TestLLMAnswerThatParsesIsAccepted(t *testing.T) {
	wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		return wp19Answer("package a\n\nvar x = 2 + 3\n"), nil
	})
	r := wp19Build(t, wp3Config)
	resp, err := r.Resolve(context.Background(), wp19Request(wp3Conflict("a.go")))
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Files[0].Content) != "package a\n\nvar x = 2 + 3\n" {
		t.Fatalf("content %q", resp.Files[0].Content)
	}
}

// TestLLMAnswerWithoutGrammarGetsMarkerRuleOnly checks that a path with no grammar is not parsed:
// any content without conflict markers is accepted.
func TestLLMAnswerWithoutGrammarGetsMarkerRuleOnly(t *testing.T) {
	wp19SetAnswer(func(context.Context, llm.Request) (*llm.Response, error) {
		return wp19Answer("package a\n\nvar x = 2 +\n"), nil
	})
	r := wp19Build(t, wp3Config)
	if _, err := r.Resolve(context.Background(), wp19Request(wp3Conflict("a.go.txt"))); err != nil {
		t.Fatalf("path without a grammar was parsed: %v", err)
	}
}

// TestOtherStrategiesAreNotSyntaxChecked checks that the syntax check belongs to the llm answers:
// a union of two Go lines that does not parse is still the union strategy's answer.
func TestOtherStrategiesAreNotSyntaxChecked(t *testing.T) {
	cfg, err := ParseConfig([]byte("[[strategy]]\ntype = \"union\"\npaths = [\"*.go\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	f := protocol.FileConflict{
		Path:   "a.go",
		Kind:   protocol.KindContent,
		Base:   wp19Side(protocol.ModeFile, "package a\n\nfunc F() {\n}\n"),
		Ours:   wp19Side(protocol.ModeFile, "package a\n\nfunc F() {\n\tx := (\n}\n"),
		Theirs: wp19Side(protocol.ModeFile, "package a\n\nfunc F() {\n\ty := [\n}\n"),
	}
	if _, err := r.Resolve(context.Background(), wp19Request(f)); err != nil {
		t.Fatalf("union answer rejected: %v", err)
	}
}
