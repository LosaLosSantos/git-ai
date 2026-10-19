//go:build cgo

package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
)

// TestRunnerCountsAnswersEqualToOurs: an accepted answer that keeps the tip's version is marked,
// and the report counts it.
func TestRunnerCountsAnswersEqualToOurs(t *testing.T) {
	f := newFixture(t)
	buildHistory(f)
	head := f.git("rev-parse", "HEAD")
	cases, _, err := Mine(context.Background(), Repo{Name: "fixture", Commit: head, License: "MIT"}, f.dir, MineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := resolve.NewResolver([]resolve.Binding{{Name: "fake", Strategy: fixedStrategy{content: []byte(goOurs)}}}, nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	results, err := Runner{Resolver: res, Clone: func(string) string { return f.dir }}.Run(context.Background(), cases)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != OutcomeWrong || !results[0].EqualsOurs {
		t.Fatalf("results %+v, want a wrong answer equal to ours", results)
	}
	rep := NewReport("ours", results)
	if rep.Total.EqualsOurs != 1 {
		t.Fatalf("totals %+v", rep.Total)
	}
	var md strings.Builder
	if err := rep.WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md.String(), "Accepted results equal to OURS (the model kept the tip's version): 1 of 1 (correct 0, wrong 1)") {
		t.Fatalf("markdown:\n%s", md.String())
	}
}

// TestRunnerReportsTooManyRegionsAsNotSent: a file with more conflicting regions than the llm
// strategy sends is declined before any call, and the bench says why.
func TestRunnerReportsTooManyRegionsAsNotSent(t *testing.T) {
	lines := func(prefix string) string {
		var b strings.Builder
		for i := 1; i <= 50; i++ {
			if i%2 == 1 && prefix != "" {
				fmt.Fprintf(&b, "%s %d\n", prefix, i)
				continue
			}
			fmt.Fprintf(&b, "line %d\n", i)
		}
		return b.String()
	}
	f := newFixture(t)
	f.commit("base", map[string]string{"a.py": lines("")})
	f.git("checkout", "-q", "-b", "feature")
	f.commit("feature", map[string]string{"a.py": lines("theirs")})
	f.git("checkout", "-q", "main")
	f.commit("main", map[string]string{"a.py": lines("ours")})
	f.conflictMerge("feature", map[string]string{"a.py": lines("human")})
	head := f.git("rev-parse", "HEAD")
	cases, _, err := Mine(context.Background(), Repo{Name: "fixture", Commit: head, License: "MIT"}, f.dir, MineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("the fixture mined %d cases, want 1", len(cases))
	}
	// The llm strategy runs a program that is never started: the file is declined first.
	cfg, err := resolve.ParseConfig([]byte("[[strategy]]\ntype = \"llm\"\n\n[strategy.llm]\ntype = \"command\"\ncommand = [\"/bin/false\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	results, err := Runner{Resolver: res, Clone: func(string) string { return f.dir }}.Run(context.Background(), cases)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != OutcomeNotSent || results[0].Reason != NotSentRegions {
		t.Fatalf("results %+v, want not sent for its regions", results)
	}
}

func TestRejectionReason(t *testing.T) {
	for in, want := range map[string]string{
		"region 2: resolve: llm answer rejected: the patched code does not start with the context lines of the code": "the patched code does not start with the context lines of the code",
		"a.go does not parse as go: line 3, column 9":                                                                "syntax error",
		"check failed: exit status 1":                                                                                "configured check",
	} {
		if got := RejectionReason(in); got != want {
			t.Errorf("RejectionReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// benchOursProvider answers every region with the CODE of the request unchanged: OURS' region.
type benchOursProvider struct{}

func (benchOursProvider) Name() string { return "bench:ours" }

var benchCodeRE = regexp.MustCompile(`(?s)===== BEGIN CODE ([0-9a-f]+) =====\n(.*?)===== END CODE ([0-9a-f]+) =====`)

func (benchOursProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	m := benchCodeRE.FindStringSubmatch(req.Prompt)
	if m == nil {
		return nil, fmt.Errorf("no CODE in the request")
	}
	n := m[1]
	return &llm.Response{Text: "===== BEGIN PATCHED CODE " + n + " =====\n" + m[2] + "===== END PATCHED CODE " + n + " =====\n", Model: "bench-ours",
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 40, ReasoningTokens: 10}}, nil
}

func init() {
	llm.Register("bench-ours-fake", func(llm.Config) (llm.Provider, error) { return benchOursProvider{}, nil })
}

// TestRunnerCountsRegionsEqualToOurs: the bench counts, per region, the answers of an llm strategy
// that keep OURS' region, besides the per-file count.
func TestRunnerCountsRegionsEqualToOurs(t *testing.T) {
	f := newFixture(t)
	buildHistory(f)
	head := f.git("rev-parse", "HEAD")
	cases, _, err := Mine(context.Background(), Repo{Name: "fixture", Commit: head, License: "MIT"}, f.dir, MineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := resolve.ParseConfig([]byte("[[strategy]]\ntype = \"llm\"\n\n[strategy.llm]\ntype = \"bench-ours-fake\"\nmodel = \"m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	results, err := Runner{Resolver: res, Clone: func(string) string { return f.dir }}.Run(context.Background(), cases)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].EqualsOurs || results[0].Regions != 1 || results[0].RegionsEqualOurs != 1 {
		t.Fatalf("results %+v, want one region answered with ours", results)
	}
	rep := NewReport("ours", results)
	var md strings.Builder
	if err := rep.WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	if results[0].InputTokens != 100 || results[0].OutputTokens != 40 || results[0].ReasoningTokens != 10 {
		t.Fatalf("tokens %+v", results[0])
	}
	if !strings.Contains(md.String(), "Tokens of the model calls, as the provider reports them: input 100, output 40, of which reasoning 10 (25.0% of the output).") {
		t.Fatalf("markdown:\n%s", md.String())
	}
	if !strings.Contains(md.String(), "Regions of the accepted model answers answered with OURS' region unchanged: 1 of 1.") {
		t.Fatalf("markdown:\n%s", md.String())
	}
}

// benchBadProvider answers every region with text that holds no patched code.
type benchBadProvider struct{}

func (benchBadProvider) Name() string { return "bench:bad" }

func (benchBadProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{Text: "I cannot do that.", Model: "bench-bad"}, nil
}

func init() {
	llm.Register("bench-bad-fake", func(llm.Config) (llm.Provider, error) { return benchBadProvider{}, nil })
}

// TestRunnerKeepsRejectedAnswers: the raw text of a rejected model answer is kept beside the
// answers, readable by its owner only, and the report counts the cases by line endings.
func TestRunnerKeepsRejectedAnswers(t *testing.T) {
	f := newFixture(t)
	buildHistory(f)
	head := f.git("rev-parse", "HEAD")
	cases, _, err := Mine(context.Background(), Repo{Name: "fixture", Commit: head, License: "MIT"}, f.dir, MineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := resolve.ParseConfig([]byte("[[strategy]]\ntype = \"llm\"\n\n[strategy.llm]\ntype = \"bench-bad-fake\"\nmodel = \"m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	answers := filepath.Join(t.TempDir(), "answers")
	if err := os.MkdirAll(answers, 0o700); err != nil {
		t.Fatal(err)
	}
	results, err := Runner{Resolver: res, Clone: func(string) string { return f.dir }, AnswersDir: answers}.Run(context.Background(), cases)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != OutcomeRejected || results[0].LineEndings != resolve.LineEndingsLF {
		t.Fatalf("results %+v, want one rejected LF case", results)
	}
	path := filepath.Join(answers, results[0].ID+".rejected.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "I cannot do that.") || !strings.Contains(string(raw), "has no patched code") {
		t.Fatalf("rejected.txt = %q", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	var md strings.Builder
	if err := NewReport("bad", results).WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md.String(), "Content conflicts by line endings (CRLF goes to the model as LF; mixed is sent as it is): lf 1 (correct 0, wrong 0, rejected 1).") {
		t.Fatalf("markdown:\n%s", md.String())
	}
}
