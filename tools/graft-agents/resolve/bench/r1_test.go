//go:build cgo

package bench

import (
	"context"
	"fmt"
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
	return &llm.Response{Text: "===== BEGIN PATCHED CODE " + n + " =====\n" + m[2] + "===== END PATCHED CODE " + n + " =====\n", Model: "bench-ours"}, nil
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
	if !strings.Contains(md.String(), "Regions of the accepted model answers answered with OURS' region unchanged: 1 of 1.") {
		t.Fatalf("markdown:\n%s", md.String())
	}
}
