package superfan

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/tools/graft-agents/notify"
)

// startedDetails blocks until its context ends, as a details command does when
// its server hangs. started is closed when the first fetch begins.
type startedDetails struct {
	started chan struct{}
	once    sync.Once
}

func (d *startedDetails) Details(ctx context.Context, _ Event) (Details, error) {
	d.once.Do(func() { close(d.started) })
	<-ctx.Done()
	return Details{}, ctx.Err()
}

// findDigest returns the digest of a component among the inputs of a summarizer.
func findDigest(t *testing.T, inputs []Digest, component string) Digest {
	t.Helper()
	for _, d := range inputs {
		if d.Component == component {
			return d
		}
	}
	t.Fatalf("no digest for %q in %+v", component, inputs)
	return Digest{}
}

func TestShutdownDuringDetailsLeavesEventUnhandled(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "cursor")
	store := NewCursorStore(cursorPath)
	details := &startedDetails{started: make(chan struct{})}
	src := &blockingSource{events: []Event{evOn(5, '5', at(1))}}
	sf, err := New(Options{
		Config:    engineConfig(),
		Source:    src,
		Details:   details,
		Notifiers: map[string]notify.Notifier{"all": &recorder{name: "all"}},
		Cursor:    store,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sf.Run(ctx) }()
	<-details.started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after cancel: %v", err)
	}
	if seq, err := store.Load(); err != nil || seq != 0 {
		t.Fatalf("cursor = %d, %v; want 0, so that the interrupted event is read again", seq, err)
	}

	// After a restart the event is analyzed in full: its protected-path
	// finding is delivered.
	rec := &recorder{name: "all"}
	det := &fakeDetails{byOID: map[string]Details{oid('5'): files("ledger/a.go")}}
	sf, err = New(Options{
		Config:    engineConfig(),
		Source:    &sliceSource{events: []Event{evOn(5, '5', at(1))}},
		Details:   det,
		Notifiers: map[string]notify.Notifier{"all": rec},
		Cursor:    store,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || len(msgs[0].Findings) != 1 || msgs[0].Findings[0].Rule != RuleProtectedPath || msgs[0].Findings[0].Sequence != 5 {
		t.Fatalf("restart did not analyze the interrupted event: %+v", msgs)
	}
}

func TestReferencesRestrictAnalysis(t *testing.T) {
	cfg := engineConfig()
	cfg.References = []string{"refs/heads/*"}
	green := evOn(2, '2', at(2))
	green.Reference = "refs/graft/green/main"
	det := &fakeDetails{byOID: map[string]Details{
		oid('1'): files("ledger/a.go"),
		oid('2'): files("ledger/b.go"),
	}}
	rec, err := runEngine(t, cfg, []Event{evOn(1, '1', at(1)), green}, det, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || msgs[0].Events != 1 || msgs[0].Findings[0].Sequence != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	if det.calls != 1 {
		t.Errorf("details fetched %d times, want 1: the green ref must not be fetched", det.calls)
	}
}

func TestDeletionIsNotFetched(t *testing.T) {
	cfg := engineConfig()
	cfg.Digest.IncludeQuiet = true
	del := evOn(1, '1', at(1))
	del.NewOID = oid('0')
	det := &fakeDetails{}
	rec, err := runEngine(t, cfg, []Event{del}, det, nil)
	if err != nil {
		t.Fatal(err)
	}
	if det.calls != 0 {
		t.Errorf("details fetched %d times for a deletion, want 0", det.calls)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || msgs[0].Component != UnassignedComponent || len(msgs[0].Findings) != 0 {
		t.Fatalf("messages = %+v", msgs)
	}
}

// changesetsWindows returns two changesets: a docs change in the first window,
// and a change of docs and ledger files in the second one. The ledger file is
// protected, so the second window has findings for both components.
func changesetsWindows() ([]Event, *fakeDetails) {
	det := &fakeDetails{byOID: map[string]Details{
		oid('1'): {Subject: "update docs", Files: files("docs/x.md").Files},
		oid('2'): {Subject: "fix ledger", Files: files("ledger/a.go", "docs/y.md").Files},
	}}
	return []Event{evOn(1, '1', at(1)), evOn(2, '2', at(70))}, det
}

func TestDigestDescribesChangesets(t *testing.T) {
	events, det := changesetsWindows()

	// Quiet windows are sent when asked for: the docs change of the first
	// window has no finding.
	cfg := engineConfig()
	cfg.Digest.IncludeQuiet = true
	sum := &fakeSummarizer{}
	rec, err := runEngine(t, cfg, events, det, func(o *Options) { o.Summarizer = sum })
	if err != nil {
		t.Fatal(err)
	}
	if got := len(rec.messages()); got != 3 {
		t.Fatalf("messages = %d, want 3 (docs quiet, docs and payments in the second window)", got)
	}
	if len(sum.inputs) != 3 {
		t.Fatalf("summarizer saw %d digests, want 3", len(sum.inputs))
	}

	quiet := sum.inputs[0]
	if quiet.Component != "docs" || quiet.Events != 1 || len(quiet.Findings) != 0 {
		t.Fatalf("quiet digest = %+v", quiet)
	}
	if e := quiet.Entries[0]; e.Sequence != 1 || e.Subject != "update docs" || strings.Join(e.Files, ",") != "docs/x.md" {
		t.Errorf("quiet digest entry = %+v", e)
	}

	docs := sum.inputs[1]
	if docs.Component != "docs" || docs.Events != 1 || len(docs.Findings) != 1 || len(docs.Entries) != 1 {
		t.Fatalf("docs digest of the second window = %+v", docs)
	}
	if e := docs.Entries[0]; e.Sequence != 2 || e.Subject != "fix ledger" || strings.Join(e.Files, ",") != "docs/y.md" {
		t.Errorf("docs entry = %+v; only the files of the component belong to it", e)
	}

	pay := findDigest(t, sum.inputs[2:], "payments")
	if len(pay.Entries) != 1 || strings.Join(pay.Entries[0].Files, ",") != "ledger/a.go" || len(pay.Findings) != 1 {
		t.Errorf("payments digest = %+v", pay)
	}

	// By default the quiet window of docs is not sent.
	events, det = changesetsWindows()
	rec, err = runEngine(t, engineConfig(), events, det, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := rec.messages()
	if len(msgs) != 2 {
		t.Fatalf("default delivered %d messages, want 2 (no quiet window): %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if len(m.Findings) == 0 {
			t.Errorf("a quiet digest was sent by default: %+v", m)
		}
	}
}

func TestDigestEntriesAreBounded(t *testing.T) {
	det := &fakeDetails{byOID: map[string]Details{oid('1'): files("ledger/a.go")}}
	var events []Event
	total := maxDigestEntries + 50
	for seq := 1; seq <= total; seq++ {
		events = append(events, evOn(uint64(seq), '1', at(1)))
	}
	sum := &fakeSummarizer{}
	if _, err := runEngine(t, engineConfig(), events, det, func(o *Options) { o.Summarizer = sum }); err != nil {
		t.Fatal(err)
	}
	d := findDigest(t, sum.inputs, "payments")
	if len(d.Entries) != maxDigestEntries || d.EntriesOmitted != 50 || d.Events != total {
		t.Fatalf("entries = %d, omitted = %d, events = %d", len(d.Entries), d.EntriesOmitted, d.Events)
	}
}

func TestCommitTextIsBoundedAndPlain(t *testing.T) {
	huge := strings.Repeat("\x1b[2J", 1<<19) // 2 MiB of escape sequences
	det := &fakeDetails{byOID: map[string]Details{oid('1'): {
		Subject: `Revert "` + huge + `"`,
		Files:   files("ledger/a.go").Files,
		Trailers: map[string][]string{
			TrailerResolvedBy: {huge},
		},
	}}}
	cfg := engineConfig()
	cfg.Rules.ProtectedPaths = nil
	sum := &fakeSummarizer{}
	rec, err := runEngine(t, cfg, []Event{evOn(1, '1', at(1))}, det, func(o *Options) { o.Summarizer = sum })
	if err != nil {
		t.Fatal(err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || len(msgs[0].Findings) != 2 {
		t.Fatalf("messages = %+v", msgs)
	}
	for _, f := range msgs[0].Findings {
		if len(f.Summary) > maxSummaryText {
			t.Errorf("finding summary is %d bytes, want at most %d", len(f.Summary), maxSummaryText)
		}
		if strings.ContainsRune(f.Summary, '\x1b') {
			t.Errorf("finding summary keeps an escape character: %q", f.Summary[:20])
		}
	}
	if e := sum.inputs[0].Entries[0]; len(e.Subject) > maxSubjectText || strings.ContainsRune(e.Subject, '\x1b') {
		t.Errorf("digest entry subject = %d bytes, plain = %v", len(e.Subject), !strings.ContainsRune(e.Subject, '\x1b'))
	}
	if text := notify.Text(msgs[0]); strings.ContainsRune(text, '\x1b') {
		t.Errorf("stdout rendering keeps an escape character")
	}
}

func TestProtectedSummaryIsBounded(t *testing.T) {
	var paths []FileChange
	for i := 0; i < 1000; i++ {
		paths = append(paths, FileChange{Path: fmt.Sprintf("ledger/%s%d.go", strings.Repeat("p", 300), i)})
	}
	a, err := NewAnalyzer(RulesConfig{ProtectedPaths: []string{"ledger/**"}})
	if err != nil {
		t.Fatal(err)
	}
	findings := a.Analyze(evOn(1, '1', at(1)), &Details{Files: paths})
	if len(findings) != 1 || findings[0].Rule != RuleProtectedPath {
		t.Fatalf("findings = %+v", findings)
	}
	f := findings[0]
	if len(f.Summary) > maxSummaryText {
		t.Errorf("summary is %d bytes", len(f.Summary))
	}
	if len(f.Paths) != maxFindingPaths {
		t.Errorf("paths kept = %d, want %d", len(f.Paths), maxFindingPaths)
	}
	// The count of the paths left out must survive the bound.
	if !strings.HasSuffix(f.Summary, " more)") {
		t.Errorf("summary does not say how many paths it leaves out: %q", f.Summary[len(f.Summary)-40:])
	}
}

func TestBurstStateIsBounded(t *testing.T) {
	a, err := NewAnalyzer(RulesConfig{Burst: &BurstRule{Lands: 3, Window: "1h"}})
	if err != nil {
		t.Fatal(err)
	}
	fired := 0
	count := func(evs []Finding) {
		for _, f := range evs {
			if f.Rule == RuleBurst {
				fired++
			}
		}
	}
	// A thousand lands within a minute: the burst fires once, at the third.
	for i := 0; i < 1000; i++ {
		count(a.Analyze(mkEvent(uint64(i+1), "land", "agent-1", at(0).Add(time.Duration(i)*time.Second)), nil))
	}
	if fired != 1 {
		t.Fatalf("burst fired %d times in the first window, want 1", fired)
	}
	if len(a.lands) > 4 {
		t.Errorf("burst state holds %d lands, want at most 4", len(a.lands))
	}
	// Three lands an hour later fire the rule again.
	for i := 0; i < 3; i++ {
		count(a.Analyze(mkEvent(uint64(2000+i), "land", "agent-1", at(120).Add(time.Duration(i)*time.Second)), nil))
	}
	if fired != 2 {
		t.Fatalf("burst fired %d times in total, want 2", fired)
	}
}
