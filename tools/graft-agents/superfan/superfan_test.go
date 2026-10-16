package superfan

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/notify"
)

// at returns base plus a duration in minutes.
func at(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

// evOn returns an event whose commit is oid(c).
func evOn(seq uint64, c byte, recorded time.Time) Event {
	ev := mkEvent(seq, "land", "agent-1", recorded)
	ev.NewOID = oid(c)
	return ev
}

func files(paths ...string) Details {
	d := Details{}
	for _, p := range paths {
		d.Files = append(d.Files, FileChange{Path: p, Additions: 1})
	}
	return d
}

// engineConfig is a configuration with two components and the protected
// ledger path, routed to the "all" notifier.
func engineConfig() *Config {
	cfg := baseConfig()
	cfg.Components = []ComponentConfig{
		{Name: "payments", Paths: []string{"ledger/**"}},
		{Name: "docs", Paths: []string{"**/*.md"}},
	}
	cfg.Notifiers = []NotifierConfig{{Name: "all", Type: "stdout"}}
	cfg.Routes = []RouteConfig{{Component: AnyComponent, Notifiers: []string{"all"}}}
	cfg.Rules.ProtectedPaths = []string{"ledger/**"}
	return cfg
}

// runEngine builds a Superfan with fake collaborators and runs it to the end
// of the source. It returns the recorder and the run error.
func runEngine(t *testing.T, cfg *Config, events []Event, det DetailSource, mod func(*Options)) (*recorder, error) {
	t.Helper()
	rec := &recorder{name: "all"}
	opts := Options{
		Config:    cfg,
		Source:    &sliceSource{events: events},
		Details:   det,
		Notifiers: map[string]notify.Notifier{"all": rec},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mod != nil {
		mod(&opts)
	}
	sf, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return rec, sf.Run(context.Background())
}

func TestDigestsPerComponentAndWindow(t *testing.T) {
	det := &fakeDetails{byOID: map[string]Details{
		oid('1'): files("ledger/a.go"),
		oid('2'): files("docs/x.md", "ledger/b.go"),
		oid('3'): files("ledger/c.go"),
	}}
	events := []Event{
		evOn(1, '1', at(5)),
		evOn(2, '2', at(20)),
		evOn(3, '3', at(90)),
	}
	rec, err := runEngine(t, engineConfig(), events, det, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := rec.messages()
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(msgs), msgs)
	}

	docs := msgs[0]
	if docs.Component != "docs" || docs.Events != 1 || !docs.WindowStart.Equal(base) || len(docs.Findings) != 1 {
		t.Errorf("docs digest = %+v", docs)
	}
	pay := msgs[1]
	if pay.Component != "payments" || pay.Events != 2 || !pay.WindowStart.Equal(base) {
		t.Errorf("payments window 1 = %+v", pay)
	}
	if len(pay.Findings) != 2 || pay.Findings[0].Sequence != 1 || pay.Findings[1].Sequence != 2 {
		t.Errorf("payments findings = %+v", pay.Findings)
	}
	if pay.Findings[0].Rule != RuleProtectedPath || pay.Findings[0].Reference != "refs/heads/main" || pay.Findings[0].Actor != "agent-1" {
		t.Errorf("finding lost event data: %+v", pay.Findings[0])
	}
	if pay.Risk != SeverityHigh {
		t.Errorf("risk = %q, want high", pay.Risk)
	}

	later := msgs[2]
	if later.Component != "payments" || later.Events != 1 || !later.WindowStart.Equal(at(60)) || !later.WindowEnd.Equal(at(120)) {
		t.Errorf("payments window 2 = %+v", later)
	}
}

func TestUnassignedWhenNoComponentMatches(t *testing.T) {
	cfg := engineConfig()
	cfg.Rules.LargeChange = &LargeChangeRule{MaxFiles: 1}
	det := &fakeDetails{byOID: map[string]Details{
		oid('1'): files("misc/a.go", "misc/b.go"),
	}}
	rec, err := runEngine(t, cfg, []Event{evOn(1, '1', at(1))}, det, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || msgs[0].Component != UnassignedComponent || msgs[0].Findings[0].Rule != RuleLargeChange {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestWithoutDetailsEverythingIsUnassigned(t *testing.T) {
	cfg := engineConfig()
	cfg.Rules.ExpectedActors = []string{"ci"}
	rec, err := runEngine(t, cfg, []Event{evOn(1, '1', at(1))}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || msgs[0].Component != UnassignedComponent || msgs[0].Findings[0].Rule != RuleUnexpectedActor {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestDetailsFailureIsAFinding(t *testing.T) {
	det := &fakeDetails{failFor: map[string]bool{oid('1'): true}}
	rec, err := runEngine(t, engineConfig(), []Event{evOn(1, '1', at(1))}, det, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || msgs[0].Findings[0].Rule != RuleDetailsUnavailable || msgs[0].Findings[0].Severity != SeverityMedium {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestRoutesSendToNamedNotifiers(t *testing.T) {
	cfg := engineConfig()
	cfg.Notifiers = []NotifierConfig{{Name: "pay", Type: "stdout"}, {Name: "all", Type: "stdout"}}
	cfg.Routes = []RouteConfig{
		{Component: "payments", Notifiers: []string{"pay", "all"}},
		{Component: AnyComponent, Notifiers: []string{"all"}},
	}
	det := &fakeDetails{byOID: map[string]Details{
		oid('1'): files("ledger/a.go"),
		oid('2'): files("docs/x.md"),
	}}
	events := []Event{evOn(1, '1', at(1)), evOn(2, '2', at(2))}

	pay := &recorder{name: "pay"}
	all := &recorder{name: "all"}
	sf, err := New(Options{
		Config:    cfg,
		Source:    &sliceSource{events: events},
		Details:   det,
		Notifiers: map[string]notify.Notifier{"pay": pay, "all": all},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(pay.messages()); got != 1 || pay.messages()[0].Component != "payments" {
		t.Errorf("pay notifier got %d messages", got)
	}
	// Only the ledger change has a finding (protected path), so only the
	// payments digest exists; it reaches both notifiers.
	if got := len(all.messages()); got != 1 {
		t.Errorf("all notifier got %d messages, want 1", got)
	}
}

func TestRouteToManyNotifiersDeduplicates(t *testing.T) {
	cfg := engineConfig()
	cfg.Notifiers = []NotifierConfig{{Name: "x", Type: "stdout"}}
	cfg.Routes = []RouteConfig{
		{Component: "payments", Notifiers: []string{"x"}},
		{Component: AnyComponent, Notifiers: []string{"x"}},
	}
	sf := &Superfan{routes: cfg.Routes}
	if got := sf.targets("payments"); len(got) != 1 || got[0] != "x" {
		t.Errorf("targets = %v", got)
	}
}

func TestAfterSkipsDeliveredEvents(t *testing.T) {
	det := &fakeDetails{byOID: map[string]Details{oid('1'): files("ledger/a.go")}}
	var events []Event
	for seq := uint64(1); seq <= 7; seq++ {
		events = append(events, evOn(seq, '1', at(int(seq))))
	}
	rec, err := runEngine(t, engineConfig(), events, det, func(o *Options) { o.After = 5 })
	if err != nil {
		t.Fatal(err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || msgs[0].Events != 2 || msgs[0].Findings[0].Sequence != 6 {
		t.Fatalf("messages = %+v", msgs)
	}
	if det.calls != 2 {
		t.Errorf("details fetched %d times, want 2", det.calls)
	}
}

func TestCursorSavedWhenWindowCloses(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "cursor")
	det := &fakeDetails{byOID: map[string]Details{oid('1'): files("ledger/a.go")}}

	events := []Event{evOn(1, '1', at(1)), evOn(2, '1', at(2))}
	_, err := runEngine(t, engineConfig(), events, det, func(o *Options) {
		o.Cursor = NewCursorStore(cursorPath)
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewCursorStore(cursorPath)
	if seq, err := store.Load(); err != nil || seq != 2 {
		t.Fatalf("cursor after run = %d, %v; want 2", seq, err)
	}

	// A restart resumes after the cursor: only the new event is processed.
	events = append(events, evOn(3, '1', at(3)))
	rec, err := runEngine(t, engineConfig(), events, det, func(o *Options) {
		o.Cursor = store
		o.After = 2
	})
	if err != nil {
		t.Fatal(err)
	}
	if msgs := rec.messages(); len(msgs) != 1 || msgs[0].Events != 1 || msgs[0].Findings[0].Sequence != 3 {
		t.Fatalf("resumed run delivered %+v", msgs)
	}
	if seq, _ := store.Load(); seq != 3 {
		t.Errorf("cursor after resume = %d, want 3", seq)
	}
}

func TestCursorNotAdvancedWithoutEvents(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "cursor")
	_, err := runEngine(t, engineConfig(), nil, nil, func(o *Options) {
		o.Cursor = NewCursorStore(cursorPath)
		o.After = 9
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cursorPath); !os.IsNotExist(err) {
		t.Errorf("cursor written although nothing was processed: %v", err)
	}
}

func TestSummarizerAddsSummaryAndRaisesRisk(t *testing.T) {
	cfg := engineConfig()
	cfg.Rules.ProtectedPaths = nil
	revertDetails := &fakeDetails{byOID: map[string]Details{oid('1'): {Subject: `Revert "x"`, Files: files("ledger/a.go").Files}}}

	sum := &fakeSummarizer{sum: Summary{Text: "two lands reverted", Risk: SeverityHigh}}
	rec, err := runEngine(t, cfg, []Event{evOn(1, '1', at(1))}, revertDetails, func(o *Options) { o.Summarizer = sum })
	if err != nil {
		t.Fatal(err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || msgs[0].Summary != "two lands reverted" || msgs[0].Risk != SeverityHigh {
		t.Fatalf("messages = %+v", msgs)
	}
	if len(sum.inputs) != 1 || sum.inputs[0].Component != "payments" || sum.inputs[0].Events != 1 {
		t.Errorf("summarizer input = %+v", sum.inputs)
	}
}

func TestSummarizerCannotLowerRisk(t *testing.T) {
	cfg := engineConfig()
	cfg.Rules.ProtectedPaths = nil
	revertDetails := &fakeDetails{byOID: map[string]Details{oid('1'): {Subject: `Revert "x"`, Files: []FileChange{{Path: "ledger/a.go"}}}}}
	sum := &fakeSummarizer{sum: Summary{Text: "fine", Risk: SeverityNone}}
	rec, err := runEngine(t, cfg, []Event{evOn(1, '1', at(1))}, revertDetails, func(o *Options) { o.Summarizer = sum })
	if err != nil {
		t.Fatal(err)
	}
	if msgs := rec.messages(); len(msgs) != 1 || msgs[0].Risk != SeverityLow {
		t.Fatalf("risk lowered by summarizer: %+v", msgs)
	}
}

func TestSummarizerFailureSendsWithoutSummary(t *testing.T) {
	det := &fakeDetails{byOID: map[string]Details{oid('1'): files("ledger/a.go")}}
	sum := &fakeSummarizer{err: errors.New("model unavailable")}
	rec, err := runEngine(t, engineConfig(), []Event{evOn(1, '1', at(1))}, det, func(o *Options) { o.Summarizer = sum })
	if err != nil {
		t.Fatalf("summarizer failure failed the run: %v", err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || msgs[0].Summary != "" || len(msgs[0].Findings) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestSummarizerInputIsBounded(t *testing.T) {
	cfg := engineConfig()
	cfg.Rules.ProtectedPaths = []string{"**"}
	var paths []FileChange
	for i := 0; i < maxDigestItems+5; i++ {
		paths = append(paths, FileChange{Path: "ledger/f" + strings.Repeat("x", i%3)})
	}
	det := &fakeDetails{byOID: map[string]Details{oid('1'): {Files: paths}}}
	sum := &fakeSummarizer{}
	var events []Event
	for seq := uint64(1); seq <= maxDigestItems+5; seq++ {
		events = append(events, evOn(seq, '1', at(1)))
	}
	rec, err := runEngine(t, cfg, events, det, func(o *Options) { o.Summarizer = sum })
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.inputs) != 1 || len(sum.inputs[0].Findings) != maxDigestItems {
		t.Fatalf("summarizer saw %d findings", len(sum.inputs[0].Findings))
	}
	msgs := rec.messages()
	if len(msgs) != 1 || len(msgs[0].Findings) != maxDigestItems || msgs[0].Omitted != 5 {
		t.Fatalf("digest bounds wrong: findings=%d omitted=%d", len(msgs[0].Findings), msgs[0].Omitted)
	}
}

func TestNotifierFailureReturnedAndCursorHeld(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "cursor")
	det := &fakeDetails{byOID: map[string]Details{oid('1'): files("ledger/a.go")}}
	rec := &recorder{name: "all", err: errors.New("503")}
	sf, err := New(Options{
		Config:    engineConfig(),
		Source:    &sliceSource{events: []Event{evOn(1, '1', at(1))}},
		Details:   det,
		Notifiers: map[string]notify.Notifier{"all": rec},
		Cursor:    NewCursorStore(cursorPath),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = sf.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `notifier "all"`) {
		t.Fatalf("Run error = %v, want notifier failure", err)
	}
	if seq, _ := NewCursorStore(cursorPath).Load(); seq != 0 {
		t.Errorf("cursor = %d, want 0: the undelivered window must be read again", seq)
	}
}

// TestUndeliveredDigestIsDeliveredAfterRestart is the regression for a digest
// lost for good: the first run cannot deliver it, the restart does.
func TestUndeliveredDigestIsDeliveredAfterRestart(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "cursor")
	det := &fakeDetails{byOID: map[string]Details{oid('1'): files("ledger/a.go")}}
	events := []Event{evOn(1, '1', at(1))}
	failing := &recorder{name: "all", err: errors.New("503")}
	first, err := New(Options{
		Config: engineConfig(), Source: &sliceSource{events: events}, Details: det,
		Notifiers: map[string]notify.Notifier{"all": failing},
		Cursor:    NewCursorStore(cursorPath),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Run(context.Background()); err == nil {
		t.Fatal("first run: want the notifier failure")
	}

	after, err := NewCursorStore(cursorPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{name: "all"}
	second, err := New(Options{
		Config: engineConfig(), Source: &sliceSource{events: events}, Details: det,
		Notifiers: map[string]notify.Notifier{"all": rec},
		Cursor:    NewCursorStore(cursorPath), After: after,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Run(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || msgs[0].Findings[0].Sequence != 1 {
		t.Fatalf("restart messages = %+v, want the digest of sequence 1", msgs)
	}
}

// flakyNotifier fails its first failures calls, then delivers to rec.
type flakyNotifier struct {
	rec      *recorder
	failures int
}

func (f *flakyNotifier) Name() string { return "all" }

func (f *flakyNotifier) Notify(ctx context.Context, msg notify.Message) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("503")
	}
	return f.rec.Notify(ctx, msg)
}

// A window that failed holds the cursor for the rest of the run, even when a
// later window is delivered.
func TestCursorStaysHeldAfterFailedWindow(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "cursor")
	det := &fakeDetails{byOID: map[string]Details{
		oid('1'): files("ledger/a.go"),
		oid('2'): files("ledger/b.go"),
	}}
	rec := &recorder{name: "all"}
	events := []Event{evOn(1, '1', at(1)), evOn(2, '2', at(61))}
	_, err := runEngine(t, engineConfig(), events, det, func(o *Options) {
		o.Notifiers = map[string]notify.Notifier{"all": &flakyNotifier{rec: rec, failures: 1}}
		o.Cursor = NewCursorStore(cursorPath)
	})
	if err == nil {
		t.Fatal("want the failure of the first window")
	}
	if got := rec.messages(); len(got) != 1 || got[0].Findings[0].Sequence != 2 {
		t.Fatalf("delivered = %+v, want only the second window", got)
	}
	if seq, _ := NewCursorStore(cursorPath).Load(); seq != 0 {
		t.Errorf("cursor = %d, want 0 while the first window is undelivered", seq)
	}
}

// Details too large to read are a large change, and the commit is filed, not
// dropped: the rule must not be quieter than a small change.
func TestOversizedDetailsAreALargeChange(t *testing.T) {
	big := CommandDetails{Argv: []string{"sh", "-c", "head -c 5000 /dev/zero"}, MaxBytes: 1024}
	rec, err := runEngine(t, engineConfig(), []Event{evOn(1, '1', at(1))}, big, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 || len(msgs[0].Findings) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	f := msgs[0].Findings[0]
	if f.Rule != RuleLargeChange || f.Severity != SeverityMedium {
		t.Errorf("finding = %s/%s, want large-change at medium", f.Rule, f.Severity)
	}
}

// A rename out of a protected path reports the old path, so the protected-path
// rule fires on it.
func TestRenameOutOfProtectedPathIsFound(t *testing.T) {
	det := &fakeDetails{byOID: map[string]Details{oid('1'): {Files: []FileChange{
		{Path: "tmp/secret.go", Status: "renamed"},
		{Path: "ledger/secret.go", Status: "renamed-from"},
	}}}}
	rec, err := runEngine(t, engineConfig(), []Event{evOn(1, '1', at(1))}, det, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := rec.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	found := false
	for _, f := range msgs[0].Findings {
		if f.Rule == RuleProtectedPath {
			found = true
		}
	}
	if !found {
		t.Errorf("no protected-path finding: %+v", msgs[0].Findings)
	}
}

func TestReadFailureDeliversThenFails(t *testing.T) {
	det := &fakeDetails{byOID: map[string]Details{oid('1'): files("ledger/a.go")}}
	src := &failingSource{events: []Event{evOn(1, '1', at(1))}, err: errors.New("pipe broke")}
	rec := &recorder{name: "all"}
	sf, err := New(Options{
		Config:    engineConfig(),
		Source:    src,
		Details:   det,
		Notifiers: map[string]notify.Notifier{"all": rec},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = sf.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pipe broke") {
		t.Fatalf("Run error = %v", err)
	}
	if len(rec.messages()) != 1 {
		t.Errorf("events read before the failure were not delivered")
	}
}

// failingSource yields events and then a read error.
type failingSource struct {
	events []Event
	err    error
}

func (s *failingSource) Next(ctx context.Context) (Event, error) {
	if len(s.events) == 0 {
		return Event{}, s.err
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

func (s *failingSource) Close() error { return nil }

// blockingSource yields events and then blocks until the context ends.
type blockingSource struct {
	events []Event
	mu     sync.Mutex
}

func (s *blockingSource) Next(ctx context.Context) (Event, error) {
	s.mu.Lock()
	if len(s.events) > 0 {
		ev := s.events[0]
		s.events = s.events[1:]
		s.mu.Unlock()
		return ev, nil
	}
	s.mu.Unlock()
	<-ctx.Done()
	return Event{}, ctx.Err()
}

func (s *blockingSource) Close() error { return nil }

func TestTickClosesElapsedWindowOnIdleStream(t *testing.T) {
	det := &fakeDetails{byOID: map[string]Details{oid('1'): files("ledger/a.go")}}
	rec := &recorder{name: "all"}
	cursorPath := filepath.Join(t.TempDir(), "cursor")
	sf, err := New(Options{
		Config:       engineConfig(),
		Source:       &blockingSource{events: []Event{evOn(1, '1', at(1))}},
		Details:      det,
		Notifiers:    map[string]notify.Notifier{"all": rec},
		Cursor:       NewCursorStore(cursorPath),
		Clock:        func() time.Time { return base.Add(3 * time.Hour) },
		TickInterval: 5 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sf.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for len(rec.messages()) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("idle window was never delivered")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after cancel: %v", err)
	}
	if seq, _ := NewCursorStore(cursorPath).Load(); seq != 1 {
		t.Errorf("cursor = %d, want 1", seq)
	}
}

func TestCancelDeliversOpenWindow(t *testing.T) {
	det := &fakeDetails{byOID: map[string]Details{oid('1'): files("ledger/a.go")}}
	rec := &recorder{name: "all"}
	sf, err := New(Options{
		Config:    engineConfig(),
		Source:    &blockingSource{events: []Event{evOn(1, '1', at(1))}},
		Details:   det,
		Notifiers: map[string]notify.Notifier{"all": rec},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sf.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after cancel: %v", err)
	}
	if len(rec.messages()) != 1 {
		t.Errorf("open window not delivered on shutdown")
	}
}

func TestNewValidatesOptions(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := &sliceSource{}
	if _, err := New(Options{Source: src, Logger: logger}); err == nil {
		t.Error("nil config accepted")
	}
	if _, err := New(Options{Config: engineConfig(), Logger: logger}); err == nil {
		t.Error("nil source accepted")
	}
	bad := engineConfig()
	bad.Window = "never"
	if _, err := New(Options{Config: bad, Source: src, Logger: logger}); err == nil {
		t.Error("invalid config accepted")
	}
	if _, err := New(Options{Config: engineConfig(), Source: src, Notifiers: map[string]notify.Notifier{}, Logger: logger}); err == nil {
		t.Error("route to a missing notifier accepted")
	}
	if _, err := New(Options{Config: engineConfig(), Source: src, Notifiers: map[string]notify.Notifier{"all": &recorder{}}, Logger: logger}); err != nil {
		t.Errorf("valid options rejected: %v", err)
	}
}

func TestNewBuildsStdoutNotifierFromConfig(t *testing.T) {
	var out bytes.Buffer
	cfg := engineConfig()
	cfg.Notifiers = []NotifierConfig{{Name: "all", Type: "stdout"}}
	det := &fakeDetails{byOID: map[string]Details{oid('1'): files("ledger/a.go")}}
	sf, err := New(Options{
		Config:  cfg,
		Source:  &sliceSource{events: []Event{evOn(1, '1', at(1))}},
		Details: det,
		Stdout:  &out,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "component=payments") {
		t.Errorf("stdout digest missing: %q", out.String())
	}
}

func TestComponentsForMatchesAnyPath(t *testing.T) {
	sf := &Superfan{components: engineConfig().Components}
	got := sf.componentsFor(&Details{Files: []FileChange{{Path: "docs/a.md"}, {Path: "ledger/x.go"}}})
	if strings.Join(got, ",") != "docs,payments" {
		t.Errorf("componentsFor = %v", got)
	}
	if got := sf.componentsFor(&Details{Files: []FileChange{{Path: "misc"}}}); len(got) != 1 || got[0] != UnassignedComponent {
		t.Errorf("componentsFor(misc) = %v", got)
	}
}

func TestTruncateText(t *testing.T) {
	if truncateText("abc", 10) != "abc" {
		t.Error("short text changed")
	}
	if got := truncateText("aé", 2); got != "a" {
		t.Errorf("cut inside rune: %q", got)
	}
	if got := truncateText("abcdef", 3); got != "abc" {
		t.Errorf("truncateText = %q", got)
	}
}
