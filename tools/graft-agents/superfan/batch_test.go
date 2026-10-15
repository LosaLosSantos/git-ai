package superfan

import (
	"strings"
	"testing"
)

// batchEvent returns a batched land whose members landed the commits oid(c) of
// commits, in order, by the actors. The batch ends at the last commit.
func batchEvent(seq uint64, actors []string, commits []byte) Event {
	ev := mkEvent(seq, OperationLandBatch, BatchActor, base)
	for i, actor := range actors {
		ev.Members = append(ev.Members, Member{
			OriginalCommit: oid('e'),
			Commit:         oid(commits[i]),
			Actor:          actor,
			Rewritten:      true,
		})
	}
	ev.NewOID = oid(commits[len(commits)-1])
	return ev
}

func TestParseEventMembers(t *testing.T) {
	line := `{"sequence":7,"reference":"refs/heads/main","old_oid":"` + oid('0') + `","new_oid":"` + oid('2') + `",` +
		`"operation":"land_batch","actor":"graft-batch","recorded_at":"2026-10-01T10:00:00Z","rewritten":true,` +
		`"members":[{"original_commit":"` + oid('e') + `","commit":"` + oid('1') + `","actor":"mallory","rewritten":true,"resolved_paths":["a.txt"]},` +
		`{"original_commit":"` + oid('2') + `","commit":"` + oid('2') + `","actor":"agent-1"}]}`
	ev, err := ParseEvent([]byte(line))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	want := []Member{
		{OriginalCommit: oid('e'), Commit: oid('1'), Actor: "mallory", Rewritten: true, ResolvedPaths: []string{"a.txt"}},
		{OriginalCommit: oid('2'), Commit: oid('2'), Actor: "agent-1"},
	}
	if len(ev.Members) != len(want) {
		t.Fatalf("members = %+v, want %+v", ev.Members, want)
	}
	for i := range want {
		got := ev.Members[i]
		if got.OriginalCommit != want[i].OriginalCommit || got.Commit != want[i].Commit || got.Actor != want[i].Actor ||
			got.Rewritten != want[i].Rewritten || strings.Join(got.ResolvedPaths, ",") != strings.Join(want[i].ResolvedPaths, ",") {
			t.Errorf("member %d = %+v, want %+v", i, got, want[i])
		}
	}

	for _, bad := range []string{
		`{"commit":"zz","actor":"a"}`,
		`{"original_commit":"zz","commit":"` + oid('1') + `"}`,
	} {
		line := `{"sequence":7,"reference":"refs/heads/main","new_oid":"` + oid('1') + `","operation":"land_batch",` +
			`"recorded_at":"2026-10-01T10:00:00Z","members":[` + bad + `]}`
		if _, err := ParseEvent([]byte(line)); err == nil {
			t.Errorf("member %s accepted", bad)
		}
	}
}

func TestBatchUnits(t *testing.T) {
	ev := batchEvent(7, []string{"mallory", "agent-1"}, []byte{'1', '2'})
	units, complete := batchUnits(ev)
	if !complete || len(units) != 2 {
		t.Fatalf("units = %+v, complete = %v", units, complete)
	}
	if u := units[0]; u.Actor != "mallory" || u.OldOID != oid('0') || u.NewOID != oid('1') || u.Members != nil || u.Sequence != 7 {
		t.Errorf("first unit = %+v", u)
	}
	if u := units[1]; u.Actor != "agent-1" || u.OldOID != oid('1') || u.NewOID != oid('2') {
		t.Errorf("second unit = %+v", u)
	}
	// A derived unit is not split again, although its operation is land_batch.
	if again, complete := batchUnits(units[0]); !complete || len(again) != 1 || again[0].Actor != "mallory" {
		t.Errorf("unit split again: %+v", again)
	}

	// The server keeps a prefix of a member list that is too large: the last
	// listed member is then not the new tip.
	cut := batchEvent(8, []string{"agent-1"}, []byte{'1', '3'})
	units, complete = batchUnits(cut)
	if complete || len(units) != 2 || units[1].part != unlistedPart || units[1].OldOID != oid('1') || units[1].NewOID != oid('3') {
		t.Errorf("cut batch units = %+v, complete = %v", units, complete)
	}

	empty := mkEvent(9, OperationLandBatch, BatchActor, base)
	if units, complete := batchUnits(empty); complete || len(units) != 1 || units[0].part != unlistedPart {
		t.Errorf("batch without members: units = %+v, complete = %v", units, complete)
	}

	single := mkEvent(10, OperationLand, "agent-1", base)
	if units, complete := batchUnits(single); !complete || len(units) != 1 || units[0].part != wholeEvent {
		t.Errorf("single land: units = %+v, complete = %v", units, complete)
	}
}

// TestAnalyzeBatchMembers is the regression test for batched lands that hid
// their requesters: the expected actors were checked against graft-batch only,
// and the members of a batch did not count toward the burst rule.
func TestAnalyzeBatchMembers(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{
		ExpectedActors: []string{"agent-*"},
		Burst:          &BurstRule{Lands: 2, Window: "1m"},
	})
	ev := batchEvent(7, []string{"mallory", "agent-1"}, []byte{'1', '2'})

	got := a.Analyze(ev, nil)
	var unexpected, bursts []Finding
	for _, f := range got {
		switch f.Rule {
		case RuleUnexpectedActor:
			unexpected = append(unexpected, f)
		case RuleBurst:
			bursts = append(bursts, f)
		default:
			t.Errorf("unexpected finding %+v", f)
		}
	}
	if len(unexpected) != 1 || unexpected[0].Event.Actor != "mallory" || unexpected[0].Event.NewOID != oid('1') ||
		!strings.Contains(unexpected[0].Summary, `"mallory"`) {
		t.Errorf("unexpected-actor findings = %+v, want one for mallory", unexpected)
	}
	for _, f := range got {
		if strings.Contains(f.Summary, BatchActor) {
			t.Errorf("finding judges the batch identity as an actor: %+v", f)
		}
	}
	if len(bursts) != 1 {
		t.Errorf("burst findings = %+v, want one: the two members are two lands within the window", bursts)
	}
}

func TestBurstOperationMatchesBatches(t *testing.T) {
	for _, tc := range []struct {
		operation string
		want      bool
	}{
		{"", true},
		{OperationLand, true},
		{OperationLandBatch, true},
		{"ff_branch", false},
	} {
		a := newTestAnalyzer(t, RulesConfig{Burst: &BurstRule{Lands: 2, Window: "1m", Operation: tc.operation}})
		got := hasRule(a.Analyze(batchEvent(1, []string{"agent-1", "agent-2"}, []byte{'1', '2'}), nil), RuleBurst) != nil
		if got != tc.want {
			t.Errorf("operation %q: burst = %v, want %v", tc.operation, got, tc.want)
		}
	}
}

func TestAnalyzeIncompleteBatch(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{ExpectedActors: []string{"agent-*"}})

	cut := batchEvent(8, []string{"agent-1"}, []byte{'1', '3'})
	got := a.Analyze(cut, nil)
	if f := hasRule(got, RuleBatchIncomplete); f == nil || f.Severity != SeverityMedium || f.Event.NewOID != oid('3') {
		t.Errorf("findings = %+v, want batch-incomplete for the tip", got)
	}
	if f := hasRule(got, RuleUnexpectedActor); f != nil {
		t.Errorf("batch identity judged as an actor: %+v", f)
	}

	empty := mkEvent(9, OperationLandBatch, BatchActor, base)
	got = a.Analyze(empty, nil)
	if hasRule(got, RuleBatchIncomplete) == nil || hasRule(got, RuleUnexpectedActor) != nil {
		t.Errorf("batch without members: findings = %v", findRules(got))
	}
}

// TestSuperfanInspectsEveryBatchMember is the regression test for batched lands
// whose details were read for the batch tip only, so that the file rules never
// saw the changes of the other members.
func TestSuperfanInspectsEveryBatchMember(t *testing.T) {
	det := &fakeDetails{byOID: map[string]Details{
		oid('1'): files("ledger/a.go"),
		oid('2'): files("docs/x.md"),
	}}
	cfg := engineConfig()
	cfg.Rules.ExpectedActors = []string{"agent-*", "mallory"}
	ev := batchEvent(1, []string{"mallory", "agent-1"}, []byte{'1', '2'})

	rec, err := runEngine(t, cfg, []Event{ev}, det, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if det.calls != 2 {
		t.Errorf("details fetched %d times, want once per member", det.calls)
	}

	payments := false
	for _, msg := range rec.messages() {
		switch msg.Component {
		case "payments":
			payments = true
			if msg.Events != 1 || len(msg.Findings) != 1 {
				t.Fatalf("payments digest = %+v", msg)
			}
			f := msg.Findings[0]
			if f.Rule != RuleProtectedPath || f.Actor != "mallory" || f.OID != oid('1') || f.Sequence != 1 {
				t.Errorf("protected-path finding = %+v, want mallory's commit", f)
			}
		case "docs":
			t.Errorf("docs digest has findings: %+v", msg)
		}
	}
	if !payments {
		t.Errorf("the protected path of the first member was not reported: %+v", rec.messages())
	}
}

func TestSuperfanIncompleteBatchIsUninspected(t *testing.T) {
	det := &fakeDetails{byOID: map[string]Details{
		oid('1'): files("docs/x.md"),
		oid('3'): files("docs/y.md"),
	}}
	cfg := engineConfig()
	cfg.Routes = append(cfg.Routes, RouteConfig{Component: "payments", Notifiers: []string{"all"}})
	ev := batchEvent(1, []string{"agent-1"}, []byte{'1', '3'})

	rec, err := runEngine(t, cfg, []Event{ev}, det, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := map[string]bool{}
	for _, msg := range rec.messages() {
		for _, f := range msg.Findings {
			if f.Rule == RuleBatchIncomplete {
				got[msg.Component] = true
			}
		}
	}
	// The unlisted changes may touch any component, like a commit too large to
	// inspect.
	if !got[UnassignedComponent] || !got["payments"] {
		t.Errorf("batch-incomplete filed under %v, want %s and payments", got, UnassignedComponent)
	}
}
