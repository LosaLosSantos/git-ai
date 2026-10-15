package superfan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/tools/graft-agents/notify"
)

// oddPathsJSON is what `graft show --json` prints for a commit that touches a
// protected file and two files whose names hold a backslash and a tab, all
// legal in git.
func oddPathsJSON(o string) []byte {
	return []byte(fmt.Sprintf(`{"oid":%q,"parents":[],"author":"a <a@x>","subject":"s","message":"s","trailers":{},`+
		`"files":[{"path":"docs/a\\b.md","additions":1,"deletions":0},`+
		`{"path":"docs/c\td.md","additions":1,"deletions":0},`+
		`{"path":"docs/e\nf.md","additions":1,"deletions":0},`+
		`{"path":"ledger/keys.go","additions":1,"deletions":0}]}`, o))
}

// jsonDetails answers details by parsing JSON documents keyed by new OID, as
// CommandDetails does with the output of `graft show --json`.
type jsonDetails map[string][]byte

func (j jsonDetails) Details(_ context.Context, ev Event) (Details, error) {
	data, ok := j[ev.NewOID]
	if !ok {
		return Details{}, errors.New("no such commit")
	}
	return ParseDetails(data, ev.NewOID)
}

func TestParseDetailsKeepsOddPaths(t *testing.T) {
	o := oid('a')
	d, err := ParseDetails(oddPathsJSON(o), o)
	if err != nil {
		t.Fatalf("ParseDetails rejected legal git paths: %v", err)
	}
	var paths []string
	for _, f := range d.Files {
		paths = append(paths, f.Path)
	}
	want := []string{`docs/a\b.md`, "docs/c\td.md", "docs/e\nf.md", "ledger/keys.go"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("paths = %q, want %q", paths, want)
	}

	// Paths git would never write are kept too: the rules decide, not the parser.
	for _, p := range []string{"/etc/x", "../x", "a//b", "./a", ""} {
		if _, err := ParseDetails(testDetailsJSON(t, o, FileChange{Path: p}), o); err != nil {
			t.Errorf("ParseDetails rejected the whole commit for path %q: %v", p, err)
		}
	}
}

// The protected component of a commit with odd paths next to a protected one
// is still notified, even when no route but the component's own exists.
func TestOddPathDoesNotHideProtectedChange(t *testing.T) {
	cfg := engineConfig()
	cfg.Routes = []RouteConfig{{Component: "payments", Notifiers: []string{"all"}}}
	o := oid('1')
	rec, err := runEngine(t, cfg, []Event{evOn(1, '1', at(1))}, jsonDetails{o: oddPathsJSON(o)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || msgs[0].Component != "payments" {
		t.Fatalf("messages = %+v, want one payments digest", msgs)
	}
	var rules []string
	for _, f := range msgs[0].Findings {
		rules = append(rules, f.Rule)
	}
	if len(rules) != 1 || rules[0] != RuleProtectedPath {
		t.Fatalf("findings = %v, want the protected-path finding only", rules)
	}
}

// Odd paths are matched, and the paths they show are plain text.
func TestOddPathsAreMatchedAndShownPlain(t *testing.T) {
	cfg := engineConfig()
	cfg.Rules.ProtectedPaths = []string{"docs/**"}
	sum := &fakeSummarizer{}
	o := oid('1')
	rec, err := runEngine(t, cfg, []Event{evOn(1, '1', at(1))}, jsonDetails{o: oddPathsJSON(o)},
		func(o *Options) { o.Summarizer = sum })
	if err != nil {
		t.Fatal(err)
	}
	docs := findDigest(t, sum.inputs, "docs")
	if len(docs.Entries) != 1 || len(docs.Entries[0].Files) != 3 {
		t.Fatalf("docs digest = %+v, want the three odd docs paths", docs)
	}
	for _, p := range docs.Entries[0].Files {
		if strings.ContainsAny(p, "\t\n") {
			t.Errorf("digest entry shows a raw control character: %q", p)
		}
	}
	protectedFound := false
	for _, m := range rec.messages() {
		for _, f := range m.Findings {
			if f.Rule != RuleProtectedPath {
				continue
			}
			protectedFound = true
			if !strings.Contains(f.Summary, `docs/a\b.md`) || strings.ContainsAny(f.Summary, "\t\n") {
				t.Errorf("protected-path summary = %q", f.Summary)
			}
		}
	}
	if !protectedFound {
		t.Error("no protected-path finding for the odd docs paths")
	}
	a, err := NewAnalyzer(cfg.Rules)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseDetails(oddPathsJSON(o), o)
	if err != nil {
		t.Fatal(err)
	}
	f := hasRule(a.Analyze(evOn(1, '1', at(1)), &d), RuleProtectedPath)
	if f == nil || len(f.Paths) != 3 {
		t.Fatalf("protected-path finding = %+v, want three paths", f)
	}
	for _, p := range f.Paths {
		if strings.ContainsAny(p, "\t\n") {
			t.Errorf("finding path keeps a control character: %q", p)
		}
	}
}

func TestMatchFileNormalizesOddSpellings(t *testing.T) {
	protected := []string{"ledger/**"}
	for _, p := range []string{
		"ledger/keys.go",
		`ledger\keys.go`,
		"./ledger/keys.go",
		"ledger//keys.go",
		"/ledger/keys.go",
		"x/../ledger/keys.go",
		"ledger/../ledger/keys.go",
	} {
		if !MatchFile(protected, p) {
			t.Errorf("MatchFile(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"docs/ledger.md", "", "ledgers/x", `docs\a.md`} {
		if MatchFile(protected, p) {
			t.Errorf("MatchFile(%q) = true, want false", p)
		}
	}
	if !MatchFile([]string{"**/*.md"}, "docs/c\td.md") || !MatchFile([]string{"docs/*"}, `docs/a\b.md`) {
		t.Error("a tab or a backslash in a name must not stop a match")
	}
}

// A path far over any usual length is matched with memory linear in its length.
func TestMatchFileLongPath(t *testing.T) {
	long := "ledger/" + strings.Repeat("a/", 200000) + "x.go"
	start := time.Now()
	if !MatchFile([]string{"ledger/**"}, long) || !MatchFile([]string{"**/x.go"}, long) {
		t.Fatal("long protected path not matched")
	}
	if MatchFile([]string{"docs/**"}, long) {
		t.Fatal("long path matched an unrelated pattern")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("matching a long path took %s", d)
	}
}

// tooLargeDetails answers every fetch with ErrDetailsTooLarge, or with another
// error when plain is set.
type tooLargeDetails struct{ plain bool }

func (d tooLargeDetails) Details(context.Context, Event) (Details, error) {
	if d.plain {
		return Details{}, errors.New("backend unavailable")
	}
	return Details{}, fmt.Errorf("%w: limit", ErrDetailsTooLarge)
}

// A commit that cannot be inspected may touch any component, so the owners of
// a component with a route of its own hear of it.
func TestUninspectedCommitReachesRoutedComponents(t *testing.T) {
	for _, tc := range []struct {
		name string
		det  tooLargeDetails
		rule string
	}{
		{"too large", tooLargeDetails{}, RuleLargeChange},
		{"unavailable", tooLargeDetails{plain: true}, RuleDetailsUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := engineConfig()
			cfg.Routes = []RouteConfig{{Component: "payments", Notifiers: []string{"all"}}}
			rec, err := runEngine(t, cfg, []Event{evOn(1, '1', at(1))}, tc.det, nil)
			if err != nil {
				t.Fatal(err)
			}
			msgs := rec.messages()
			if len(msgs) != 1 || msgs[0].Component != "payments" || len(msgs[0].Findings) != 1 || msgs[0].Findings[0].Rule != tc.rule {
				t.Fatalf("messages = %+v, want one payments digest with a %s finding", msgs, tc.rule)
			}
		})
	}

	// Components without a route of their own are left out; "*" routes see the
	// unassigned digest.
	cfg := engineConfig()
	cfg.Routes = []RouteConfig{
		{Component: AnyComponent, Notifiers: []string{"all"}},
		{Component: "payments", Notifiers: []string{"all"}},
	}
	sf, err := New(Options{Config: cfg, Source: &sliceSource{}, Notifiers: map[string]notify.Notifier{"all": &recorder{name: "all"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sf.uninspectedComponents(), ","); got != "payments,unassigned" {
		t.Errorf("uninspected components = %s, want payments,unassigned", got)
	}
}

func TestDetailsOfAnAnnotatedTag(t *testing.T) {
	tag, commit := oid('7'), oid('c')
	data := mustJSON(t, Details{OID: commit, PeeledFrom: tag, Files: files("ledger/a.go").Files})
	d, err := ParseDetails(data, tag)
	if err != nil {
		t.Fatalf("details of a peeled tag rejected: %v", err)
	}
	if d.OID != commit || len(d.Files) != 1 {
		t.Errorf("details = %+v", d)
	}
	if _, err := ParseDetails(data, oid('8')); err == nil {
		t.Error("details of another tag accepted")
	}
	if _, err := ParseDetails(mustJSON(t, Details{OID: commit}), tag); err == nil {
		t.Error("details of another commit accepted without peeled_from")
	}
}

// burstFindings runs an analyzer over operations at the given offsets from
// base and returns the burst findings.
func burstFindings(t *testing.T, a *Analyzer, offsets []time.Duration) []Finding {
	t.Helper()
	var out []Finding
	for i, off := range offsets {
		for _, f := range a.Analyze(mkEvent(uint64(i+1), "land", "a", base.Add(off)), nil) {
			if f.Rule == RuleBurst {
				out = append(out, f)
			}
		}
	}
	return out
}

// every returns the offsets of a steady rate: one operation per gap for d.
func every(gap, d time.Duration) []time.Duration {
	var out []time.Duration
	for off := time.Duration(0); off < d; off += gap {
		out = append(out, off)
	}
	return out
}

// A sustained burst is reported once per digest window, whatever its rate:
// at the threshold rate (3 per minute) it is not reported at every operation,
// and at twice the rate it is not reported only once.
func TestBurstIsReportedOncePerDigestWindow(t *testing.T) {
	jittered := func() []time.Duration {
		var out []time.Duration
		for off, i := time.Duration(0), 0; off < 3*time.Hour; i++ {
			out = append(out, off)
			if i%2 == 0 {
				off += 25 * time.Second
			} else {
				off += 15 * time.Second
			}
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		offsets []time.Duration
	}{
		{"three per minute", every(20*time.Second, 3*time.Hour)},
		{"six per minute", every(10*time.Second, 3*time.Hour)},
		{"three per minute with jitter", jittered()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAnalyzer(t, RulesConfig{Burst: &BurstRule{Lands: 3, Window: "1m"}})
			a.SetReportWindow(time.Hour)
			got := burstFindings(t, a, tc.offsets)
			if len(got) != 3 {
				t.Fatalf("burst fired %d times in 3h, want 3 (one per hourly digest)", len(got))
			}
			for i, f := range got {
				if want := base.Add(time.Duration(i) * time.Hour); f.Event.RecordedAt.Truncate(time.Hour) != want {
					t.Errorf("finding %d is in the window of %s, want %s", i, f.Event.RecordedAt, want)
				}
				if ongoing := strings.Contains(f.Summary, "continues"); ongoing != (i > 0) {
					t.Errorf("finding %d summary = %q", i, f.Summary)
				}
			}
		})
	}
}

// A burst that stops for more than a window and starts again is a new burst,
// reported even within the same digest window.
func TestBurstAfterAPauseIsNew(t *testing.T) {
	a := newTestAnalyzer(t, RulesConfig{Burst: &BurstRule{Lands: 3, Window: "1m"}})
	a.SetReportWindow(time.Hour)
	offsets := append(every(10*time.Second, time.Minute), every(10*time.Second, time.Minute)...)
	for i := 6; i < len(offsets); i++ {
		offsets[i] += 10 * time.Minute
	}
	got := burstFindings(t, a, offsets)
	if len(got) != 2 || strings.Contains(got[1].Summary, "continues") {
		t.Fatalf("burst findings = %+v, want two separate bursts", got)
	}
}

// End to end: every hourly digest of a sustained burst carries one burst
// finding.
func TestSustainedBurstInEveryDigest(t *testing.T) {
	cfg := engineConfig()
	cfg.Rules.Burst = &BurstRule{Lands: 3, Window: "1m"}
	var events []Event
	for i, off := range every(20*time.Second, 3*time.Hour) {
		events = append(events, mkEvent(uint64(i+1), "land", "agent-1", base.Add(off)))
	}
	rec, err := runEngine(t, cfg, events, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := rec.messages()
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 hourly digests", len(msgs))
	}
	for _, m := range msgs {
		if len(m.Findings) != 1 || m.Findings[0].Rule != RuleBurst {
			t.Errorf("digest %s has findings %+v, want one burst finding", m.WindowStart, m.Findings)
		}
	}
}

func TestOneLineRemovesFormatCharacters(t *testing.T) {
	rlo, zwsp, lsep := string(rune(0x202e)), string(rune(0x200b)), string(rune(0x2028))
	got := oneLine("Fix "+rlo+"exe.txt"+zwsp+lsep, 100)
	if got != "Fix  exe.txt  " {
		t.Errorf("oneLine = %q", got)
	}
	if got := multiLine("a"+rlo+"\nb", 100); got != "a \nb" {
		t.Errorf("multiLine = %q", got)
	}
}
