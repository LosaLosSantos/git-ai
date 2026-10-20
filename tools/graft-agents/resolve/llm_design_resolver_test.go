//go:build cgo

package resolve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

// The test of the design step that goes through the llm strategy (the resolver builds it, and the
// strategy needs the tree-sitter grammars: cgo); the rest of the design tests build without cgo.

// TestLLMDesignAtTheResolver: a model answer that breaks the rules of an answer is an attempt of the
// rejected kind, the composition report goes with the resolution, and the options of the ablations are gone.
func TestLLMDesignAtTheResolver(t *testing.T) {
	file := protocol.FileConflict{
		Path: "x.txt", Kind: protocol.KindContent,
		Base:   wp19Side(protocol.ModeFile, numbered("line", 12)),
		Ours:   wp19Side(protocol.ModeFile, strings.Replace(numbered("line", 12), "line 5\n", "O5\n", 1)),
		Theirs: wp19Side(protocol.ModeFile, strings.Replace(numbered("line", 12), "line 6\n", "T6\n", 1)),
	}
	wp19SetAnswer(func(_ context.Context, req llm.Request) (*llm.Response, error) {
		return llmRawAnswer("I will not."), nil
	})
	r := wp19Build(t, wp19LLMConfig)
	_, err := r.Resolve(context.Background(), wp19Request(file))
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) || len(unresolved.Failures) != 1 {
		t.Fatalf("err = %v", err)
	}
	attempts := unresolved.Failures[0].Attempts
	if len(attempts) != 1 || attempts[0].Kind != AttemptRejected || !errors.Is(attempts[0].Err, ErrLLMRejected) {
		t.Fatalf("attempts %+v, want one rejection", attempts)
	}

	// The report goes with the resolution, and a model's resolution is marked as one.
	wp19SetAnswer(func(_ context.Context, req llm.Request) (*llm.Response, error) {
		return wp19Answer("O5\nT6\n"), nil
	})
	r = wp19Build(t, wp19LLMConfig)
	resp, err := r.Resolve(context.Background(), wp19Request(file))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Files[0]
	if got.Origin != protocol.OriginModel || !got.Proposal || fmt.Sprint(got.Report) != "[keeps every uncontested change of both sides (2)]" {
		t.Fatalf("file %+v", got)
	}

	// The elements that the measurements rejected, or made the rule, are no options any more.
	for _, gone := range []string{"symmetric_context", "abstain", "repair", "repair_composition", "git_regions", "max_context_bytes"} {
		value := "false"
		if gone == "max_context_bytes" {
			value = "4096"
		}
		if _, err := wp19BuildErr(strings.Replace(wp19LLMConfig, `paths = ["*.txt", "**/*.txt"]`, "paths = [\"*.txt\"]\n"+gone+" = "+value, 1)); err == nil {
			t.Errorf("the option %s is still accepted", gone)
		}
	}
}

// wp19BuildStrategy builds the resolver of config and returns its llm strategy.
func wp19BuildStrategy(t *testing.T, config string) *llmStrategy {
	t.Helper()
	r := wp19Build(t, config)
	for _, b := range r.bindings {
		if s, ok := b.Strategy.(*llmStrategy); ok {
			return s
		}
	}
	t.Fatal("no llm strategy in the resolver")
	return nil
}
