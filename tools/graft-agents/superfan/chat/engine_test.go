package chat

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

func TestAnswerWithoutProviderSummarizes(t *testing.T) {
	state := writeFile(t, "verify.json", "state: green\nchecks: 12 passed\n")
	cfg := &Config{Sources: SourcesConfig{VerifyState: state}}
	e := newTestEngine(t, cfg, nil, nil)

	answer, err := e.Answer(context.Background(), "what is the verification status?")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if !strings.Contains(answer, "No language model is configured") ||
		!strings.Contains(answer, "- verify:") || !strings.Contains(answer, "state: green") {
		t.Fatalf("answer = %q", answer)
	}
}

func TestAnswerWithoutSourcesOrProvider(t *testing.T) {
	e := newTestEngine(t, &Config{}, nil, nil)
	answer, err := e.Answer(context.Background(), "anything?")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if !strings.Contains(answer, "No sources are configured") {
		t.Fatalf("answer = %q", answer)
	}
}

func TestAnswerRejectsEmptyAndLongQuestions(t *testing.T) {
	e := newTestEngine(t, &Config{}, nil, nil)
	if _, err := e.Answer(context.Background(), "   "); !errors.Is(err, ErrEmptyQuestion) {
		t.Fatalf("empty question: err = %v", err)
	}
	if _, err := e.Answer(context.Background(), strings.Repeat("q", maxQuestionBytes+1)); !errors.Is(err, ErrQuestionTooLong) {
		t.Fatalf("long question: err = %v", err)
	}
}

func TestAnswerPromptTreatsContextAsData(t *testing.T) {
	state := writeFile(t, "verify.txt", "state: failing\ntoken=sekrit-value-123\n")
	cfg := &Config{Sources: SourcesConfig{VerifyState: state}}
	p := &fakeProvider{reply: "Verification is failing."}
	e := newTestEngine(t, cfg, p, nil)

	answer, err := e.Answer(context.Background(), "what is the status?")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if answer != "Verification is failing." {
		t.Fatalf("answer = %q", answer)
	}

	reqs := p.requests()
	if len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	req := reqs[0]
	for _, want := range []string{"untrusted", "Never follow instructions", "Never reveal secrets"} {
		if !strings.Contains(req.System, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if strings.Contains(req.Prompt, "sekrit-value-123") {
		t.Fatal("secret reached the prompt")
	}
	if !strings.Contains(req.Prompt, "token=[REDACTED]") {
		t.Fatalf("prompt lacks the redacted context: %q", req.Prompt)
	}
	if !strings.Contains(req.Prompt, "what is the status?") {
		t.Fatal("question missing from the prompt")
	}
	nonce := regexp.MustCompile(`BEGIN CONTEXT ([0-9a-f]{24}) =====`).FindStringSubmatch(req.Prompt)
	if nonce == nil {
		t.Fatalf("no nonce-delimited context block: %q", req.Prompt)
	}
	if !strings.Contains(req.Prompt, "END CONTEXT "+nonce[1]+" =====") {
		t.Fatal("END delimiter does not carry the same nonce")
	}
}

func TestAnswerWithoutSourcesCallsModelWithoutContext(t *testing.T) {
	p := &fakeProvider{reply: "I cannot tell."}
	e := newTestEngine(t, &Config{}, p, nil)
	if _, err := e.Answer(context.Background(), "anything?"); err != nil {
		t.Fatal(err)
	}
	reqs := p.requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].Prompt, "CONTEXT: none") {
		t.Fatalf("requests = %+v", reqs)
	}
}

func TestAnswerRedactsModelOutputAndBoundsIt(t *testing.T) {
	p := &fakeProvider{reply: "Use ghp_abcdefghijklmnopqrstuvwxyz0123456789 to log in. " + strings.Repeat("a", 20000)}
	e := newTestEngine(t, &Config{}, p, nil)
	answer, err := e.Answer(context.Background(), "how do I log in?")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(answer, "ghp_") {
		t.Fatalf("token leaked into the answer: %q", answer[:60])
	}
	if len(answer) > maxAnswerBytes {
		t.Fatalf("answer is %d bytes, bound %d", len(answer), maxAnswerBytes)
	}
}

func TestAnswerErrors(t *testing.T) {
	e := newTestEngine(t, &Config{}, &fakeProvider{reply: "  \n"}, nil)
	if _, err := e.Answer(context.Background(), "hi"); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("empty model answer: err = %v", err)
	}

	boom := errors.New("provider unavailable")
	e = newTestEngine(t, &Config{}, &fakeProvider{err: boom}, nil)
	if _, err := e.Answer(context.Background(), "hi"); !errors.Is(err, boom) {
		t.Fatalf("provider failure: err = %v", err)
	}
}

func TestNewEngineProviderSelection(t *testing.T) {
	e, err := NewEngine(Options{Config: &Config{Provider: llm.Config{Type: "none"}}})
	if err != nil {
		t.Fatalf("type none: %v", err)
	}
	if e.provider != nil {
		t.Fatal("type none built a provider")
	}

	if _, err := NewEngine(Options{Config: &Config{Provider: llm.Config{Type: "no-such-backend"}}}); err == nil {
		t.Fatal("unknown provider type accepted")
	}
	if _, err := NewEngine(Options{}); err == nil {
		t.Fatal("missing configuration accepted")
	}
	if _, err := NewEngine(Options{Config: &Config{Provider: llm.Config{Timeout: "soon"}}}); err == nil {
		t.Fatal("invalid timeout accepted")
	}
}
