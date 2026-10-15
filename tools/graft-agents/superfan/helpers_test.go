package superfan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/tools/graft-agents/notify"
)

var base = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

// oid returns a valid 40-digit object ID made of one repeated hex digit.
func oid(c byte) string { return strings.Repeat(string(c), 40) }

func mkEvent(seq uint64, op, actor string, at time.Time) Event {
	return Event{
		Sequence:   seq,
		Reference:  "refs/heads/main",
		OldOID:     oid('0'),
		NewOID:     oid('a'),
		Operation:  op,
		Actor:      actor,
		RecordedAt: at,
	}
}

// eventsDoc renders events as JSON lines, as graft watch --json does.
func eventsDoc(t testing.TB, events ...Event) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// fakeDetails answers details from a map keyed by new OID.
type fakeDetails struct {
	mu      sync.Mutex
	byOID   map[string]Details
	failFor map[string]bool
	calls   int
}

func (f *fakeDetails) Details(_ context.Context, ev Event) (Details, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failFor[ev.NewOID] {
		return Details{}, errors.New("backend unavailable")
	}
	d, ok := f.byOID[ev.NewOID]
	if !ok {
		return Details{}, errors.New("no such commit")
	}
	return d, nil
}

// sliceSource yields a fixed list of events, then io.EOF.
type sliceSource struct {
	events []Event
	next   int
	closed bool
}

func (s *sliceSource) Next(ctx context.Context) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	if s.next >= len(s.events) {
		return Event{}, io.EOF
	}
	ev := s.events[s.next]
	s.next++
	return ev, nil
}

func (s *sliceSource) Close() error {
	s.closed = true
	return nil
}

// recorder is a notifier that keeps every message it received.
type recorder struct {
	name string
	mu   sync.Mutex
	msgs []notify.Message
	err  error
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Notify(_ context.Context, msg notify.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.msgs = append(r.msgs, msg)
	return nil
}

func (r *recorder) messages() []notify.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Message(nil), r.msgs...)
}

// fakeSummarizer returns a fixed answer or error.
type fakeSummarizer struct {
	sum    Summary
	err    error
	inputs []Digest
}

func (f *fakeSummarizer) Summarize(_ context.Context, d Digest) (Summary, error) {
	f.inputs = append(f.inputs, d)
	return f.sum, f.err
}

// baseConfig is a valid configuration with one component and one notifier.
func baseConfig() *Config {
	return &Config{
		Window:     "1h",
		Components: []ComponentConfig{{Name: "payments", Paths: []string{"ledger/**"}}},
		Notifiers:  []NotifierConfig{{Name: "console", Type: "stdout"}},
		Routes:     []RouteConfig{{Component: AnyComponent, Notifiers: []string{"console"}}},
	}
}
