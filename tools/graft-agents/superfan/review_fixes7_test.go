package superfan

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/tools/graft-agents/notify"
)

// filteredEvent is an event on a reference that engineConfig with
// References = refs/heads/main filters out.
func filteredEvent(seq uint64, recorded time.Time) Event {
	ev := evOn(seq, '1', recorded)
	ev.Reference = "refs/heads/graft/agent/tmp"
	return ev
}

// A run whose events are all filtered out by "references" opens no window,
// but it must still save its progress. Otherwise a watch command that prints a
// bounded page after the cursor (graft log --json --after {after}) reads the
// same page of filtered events forever.
func TestCursorSavedForFilteredOnlyRun(t *testing.T) {
	cfg := engineConfig()
	cfg.References = []string{"refs/heads/main"}
	det := &fakeDetails{byOID: map[string]Details{oid('1'): files("ledger/a.go")}}
	var events []Event
	for seq := uint64(1); seq <= 5; seq++ {
		events = append(events, filteredEvent(seq, at(int(seq))))
	}
	cursorPath := filepath.Join(t.TempDir(), "cursor")
	rec, err := runEngine(t, cfg, events, det, func(o *Options) {
		o.Cursor = NewCursorStore(cursorPath)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if msgs := rec.messages(); len(msgs) != 0 {
		t.Errorf("filtered events delivered digests: %+v", msgs)
	}
	if det.calls != 0 {
		t.Errorf("details fetched %d times for filtered events, want 0", det.calls)
	}
	if seq, err := NewCursorStore(cursorPath).Load(); err != nil || seq != 5 {
		t.Fatalf("cursor = %d, %v; want 5", seq, err)
	}

	// The next run resumes after the filtered events.
	events = append(events, evOn(6, '1', at(6)))
	rec, err = runEngine(t, cfg, events, det, func(o *Options) {
		o.Cursor = NewCursorStore(cursorPath)
		o.After = 5
	})
	if err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	if msgs := rec.messages(); len(msgs) != 1 || msgs[0].Events != 1 || msgs[0].Findings[0].Sequence != 6 {
		t.Fatalf("resumed run delivered %+v", msgs)
	}
	if seq, _ := NewCursorStore(cursorPath).Load(); seq != 6 {
		t.Errorf("cursor after resume = %d, want 6", seq)
	}
}

// With a tick, the progress of filtered events is saved while the stream is
// idle, not only at the end of the run.
func TestTickSavesCursorForFilteredEvents(t *testing.T) {
	cfg := engineConfig()
	cfg.References = []string{"refs/heads/main"}
	cursorPath := filepath.Join(t.TempDir(), "cursor")
	sf, err := New(Options{
		Config:       cfg,
		Source:       &blockingSource{events: []Event{filteredEvent(1, at(1)), filteredEvent(2, at(2)), filteredEvent(3, at(3))}},
		Notifiers:    map[string]notify.Notifier{"all": &recorder{name: "all"}},
		Cursor:       NewCursorStore(cursorPath),
		TickInterval: 5 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sf.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if seq, _ := NewCursorStore(cursorPath).Load(); seq == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cursor never saved for filtered events on tick")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after cancel: %v", err)
	}
}

// renames returns details with n renamed files: each is a "renamed" entry for
// the new path and a "renamed-from" entry for the old path, as graft show
// --json prints them.
func renames(n int) Details {
	var d Details
	for i := 0; i < n; i++ {
		d.Files = append(d.Files,
			FileChange{Path: fmt.Sprintf("new/%d.go", i), Status: "renamed"},
			FileChange{Path: fmt.Sprintf("old/%d.go", i), Status: StatusRenamedFrom},
		)
	}
	return d
}

func TestChangedFilesCountsRenameOnce(t *testing.T) {
	d := renames(3)
	d.Files = append(d.Files, FileChange{Path: "a.go", Additions: 1})
	if got := d.ChangedFiles(); got != 4 {
		t.Errorf("ChangedFiles = %d, want 4", got)
	}
}

// A rename is one changed file for the large-change rule, although graft show
// lists both of its paths.
func TestLargeChangeCountsRenameOnce(t *testing.T) {
	a, err := NewAnalyzer(RulesConfig{LargeChange: &LargeChangeRule{MaxFiles: 50}})
	if err != nil {
		t.Fatal(err)
	}
	ev := mkEvent(1, "land", "agent-1", at(1))

	det := renames(26)
	if got := a.Analyze(ev, &det); len(got) != 0 {
		t.Errorf("26 renames under a 50-file limit: findings %+v", got)
	}

	det = renames(51)
	got := a.Analyze(ev, &det)
	if len(got) != 1 || got[0].Rule != RuleLargeChange {
		t.Fatalf("51 renames over a 50-file limit: findings %+v", got)
	}
	if !strings.HasPrefix(got[0].Summary, "51 files ") {
		t.Errorf("summary = %q, want 51 files", got[0].Summary)
	}
}
