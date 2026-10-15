package superfan

import (
	"strings"
	"testing"
	"time"
)

func findRules(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

func hasRule(fs []Finding, rule string) *Finding {
	for i := range fs {
		if fs[i].Rule == rule {
			return &fs[i]
		}
	}
	return nil
}

func newTestAnalyzer(t *testing.T, rules RulesConfig) *Analyzer {
	t.Helper()
	a, err := NewAnalyzer(rules)
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	return a
}

func TestLargeChange(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{LargeChange: &LargeChangeRule{MaxFiles: 2, MaxLines: 10}})
	ev := mkEvent(1, "land", "a", base)

	small := &Details{Files: []FileChange{{Path: "a", Additions: 3}, {Path: "b", Deletions: 2}}}
	if got := a.Analyze(ev, small); hasRule(got, RuleLargeChange) != nil {
		t.Errorf("small change flagged: %v", findRules(got))
	}

	manyFiles := &Details{Files: []FileChange{{Path: "a"}, {Path: "b"}, {Path: "c"}}}
	if f := hasRule(a.Analyze(ev, manyFiles), RuleLargeChange); f == nil || f.Severity != SeverityMedium {
		t.Errorf("3 files over max 2 not flagged: %v", findRules(a.Analyze(ev, manyFiles)))
	}

	manyLines := &Details{Files: []FileChange{{Path: "a", Additions: 11}}}
	if f := hasRule(a.Analyze(ev, manyLines), RuleLargeChange); f == nil || !strings.Contains(f.Summary, "11 changed lines") {
		t.Errorf("11 lines over max 10 not flagged")
	}

	if got := a.Analyze(ev, nil); hasRule(got, RuleLargeChange) != nil {
		t.Error("large-change evaluated without details")
	}

	onlyLines := newTestAnalyzer(t, RulesConfig{LargeChange: &LargeChangeRule{MaxLines: 5}})
	if got := onlyLines.Analyze(ev, &Details{Files: make([]FileChange, 500)}); hasRule(got, RuleLargeChange) != nil {
		t.Error("file count checked although max_files is off")
	}
}

func TestRevertAndAutoRevert(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{})
	ev := mkEvent(1, "land", "a", base)

	gitRevert := &Details{Subject: `Revert "add x"`}
	if f := hasRule(a.Analyze(ev, gitRevert), RuleRevert); f == nil || f.Severity != SeverityLow {
		t.Errorf("subject revert not flagged")
	}

	bodyRevert := &Details{Subject: "undo", Message: "undo\n\nThis reverts commit " + oid('e') + ".\n"}
	if hasRule(a.Analyze(ev, bodyRevert), RuleRevert) == nil {
		t.Errorf("git revert body not flagged")
	}

	auto := &Details{
		Subject:  `Revert "bad"`,
		Trailers: map[string][]string{TrailerAutoRevert: {oid('f')}},
	}
	got := a.Analyze(ev, auto)
	if hasRule(got, RuleAutoRevert) == nil || hasRule(got, RuleRevert) != nil {
		t.Errorf("auto-revert should replace revert, got %v", findRules(got))
	}

	plain := &Details{Subject: "feature", Message: "feature\n"}
	if got := a.Analyze(ev, plain); len(got) != 0 {
		t.Errorf("plain commit flagged: %v", findRules(got))
	}
}

func TestAIResolvedTrailer(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{})
	ev := mkEvent(1, "land", "a", base)
	det := &Details{Trailers: map[string][]string{TrailerResolvedBy: {"graft-resolve/llm"}}}
	f := hasRule(a.Analyze(ev, det), RuleAIResolved)
	if f == nil || !strings.Contains(f.Summary, "graft-resolve/llm") {
		t.Errorf("resolved trailer not flagged")
	}
}

func TestProtectedPaths(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{ProtectedPaths: []string{"ledger/**", "*.lock"}})
	ev := mkEvent(1, "land", "a", base)
	det := &Details{Files: []FileChange{
		{Path: "ledger/db/schema.sql"},
		{Path: "docs/x.md"},
		{Path: "go.lock"},
	}}
	f := hasRule(a.Analyze(ev, det), RuleProtectedPath)
	if f == nil || f.Severity != SeverityHigh {
		t.Fatalf("protected change not flagged")
	}
	if len(f.Paths) != 2 || f.Paths[0] != "ledger/db/schema.sql" || f.Paths[1] != "go.lock" {
		t.Errorf("paths = %v", f.Paths)
	}
	if got := a.Analyze(ev, &Details{Files: []FileChange{{Path: "docs/x.md"}}}); hasRule(got, RuleProtectedPath) != nil {
		t.Error("unprotected change flagged")
	}
}

func TestUnexpectedActor(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{ExpectedActors: []string{"agent-*", "ci"}})
	if got := a.Analyze(mkEvent(1, "land", "agent-9", base), nil); hasRule(got, RuleUnexpectedActor) != nil {
		t.Error("expected actor flagged")
	}
	if got := a.Analyze(mkEvent(2, "land", "ci", base), nil); hasRule(got, RuleUnexpectedActor) != nil {
		t.Error("exact expected actor flagged")
	}
	f := hasRule(a.Analyze(mkEvent(3, "land", "mallory", base), nil), RuleUnexpectedActor)
	if f == nil || f.Severity != SeverityHigh {
		t.Errorf("unexpected actor not flagged")
	}

	none := newTestAnalyzer(t, RulesConfig{})
	if got := none.Analyze(mkEvent(4, "land", "anyone", base), nil); len(got) != 0 {
		t.Errorf("no expected actors should disable the rule: %v", findRules(got))
	}
}

func TestBurstFiresOnCrossingOnly(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{Burst: &BurstRule{Lands: 3, Window: "1m"}})
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }

	var fired []uint64
	for i, sec := range []int{0, 10, 20, 30, 40} {
		for _, f := range a.Analyze(mkEvent(uint64(i+1), "land", "a", at(sec)), nil) {
			if f.Rule == RuleBurst {
				fired = append(fired, f.Event.Sequence)
			}
		}
	}
	if len(fired) != 1 || fired[0] != 3 {
		t.Fatalf("burst fired at sequences %v, want [3]", fired)
	}

	// The lands at 0..40 keep the window above 3 until the early ones age out;
	// after a quiet period the rule can fire again.
	var again []uint64
	for i, sec := range []int{200, 210, 220} {
		for _, f := range a.Analyze(mkEvent(uint64(10+i), "land", "a", at(sec)), nil) {
			if f.Rule == RuleBurst {
				again = append(again, f.Event.Sequence)
			}
		}
	}
	if len(again) != 1 || again[0] != 12 {
		t.Errorf("burst after quiet period fired at %v, want [12]", again)
	}
}

func TestBurstIgnoresOtherOperations(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{Burst: &BurstRule{Lands: 2, Window: "1h", Operation: "ff_branch"}})
	for i := 0; i < 5; i++ {
		if got := a.Analyze(mkEvent(uint64(i+1), "land", "a", base), nil); hasRule(got, RuleBurst) != nil {
			t.Fatal("land counted for an ff_branch burst")
		}
	}
	got := a.Analyze(mkEvent(9, "ff_branch", "a", base), nil)
	got = append(got, a.Analyze(mkEvent(10, "ff_branch", "a", base), nil)...)
	if hasRule(got, RuleBurst) == nil {
		t.Error("ff_branch burst not flagged")
	}
}

func TestBurstDefaultOperationIsLand(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{Burst: &BurstRule{Lands: 2, Window: "1h"}})
	a.Analyze(mkEvent(1, "land", "a", base), nil)
	if hasRule(a.Analyze(mkEvent(2, "land", "a", base), nil), RuleBurst) == nil {
		t.Error("default operation is not land")
	}
}

func TestNewAnalyzerRejectsInvalidRules(t *testing.T) {
	if _, err := NewAnalyzer(RulesConfig{Burst: &BurstRule{Lands: 1, Window: "1m"}}); err == nil {
		t.Error("burst with one land accepted")
	}
	if _, err := NewAnalyzer(RulesConfig{ProtectedPaths: []string{"../x"}}); err == nil {
		t.Error("escaping protected path accepted")
	}
}

func TestDetailsRulesNeedNoDetailsForEventRules(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{
		ProtectedPaths: []string{"x/**"},
		LargeChange:    &LargeChangeRule{MaxFiles: 1},
	})
	if got := a.Analyze(mkEvent(1, "land", "a", base), nil); len(got) != 0 {
		t.Errorf("file rules ran without details: %v", findRules(got))
	}
}

func TestSeverityHelpers(t *testing.T) {
	if severityRank(SeverityHigh) <= severityRank(SeverityMedium) || severityRank(SeverityMedium) <= severityRank(SeverityLow) {
		t.Error("severity ranks not ordered")
	}
	if severityRank("bogus") != 0 {
		t.Error("unknown severity should rank as none")
	}
	if maxRisk("", "") != SeverityNone {
		t.Error("maxRisk of empties should be none")
	}
	if maxRisk(SeverityLow, SeverityHigh) != SeverityHigh || maxRisk(SeverityHigh, SeverityLow) != SeverityHigh {
		t.Error("maxRisk wrong")
	}
	if !validRisk(SeverityNone) || validRisk("critical") {
		t.Error("validRisk wrong")
	}
}
