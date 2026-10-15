package superfan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestParseEventValid(t *testing.T) {
	line := []byte(`{"sequence":3,"reference":"refs/heads/main","old_oid":"` + oid('0') +
		`","new_oid":"` + oid('b') + `","operation":"land","actor":"agent-1",` +
		`"recorded_at":"2026-10-01T10:00:00Z","rewritten":true,"future_field":42}`)
	ev, err := ParseEvent(line)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if ev.Sequence != 3 || ev.Reference != "refs/heads/main" || ev.Operation != "land" ||
		ev.Actor != "agent-1" || !ev.Rewritten || ev.NewOID != oid('b') {
		t.Errorf("decoded event = %+v", ev)
	}
}

func TestParseEventRejectsInvalid(t *testing.T) {
	good := mkEvent(1, "land", "a", base)
	mutate := func(f func(*Event)) Event {
		ev := good
		f(&ev)
		return ev
	}
	raw := func(s string) []byte { return []byte(s) }
	for _, tc := range []struct {
		name string
		line []byte
	}{
		{"not json", raw(`{"sequence":`)},
		{"trailing data", raw(`{"sequence":1} {"sequence":2}`)},
		{"no sequence", mustJSON(t, mutate(func(e *Event) { e.Sequence = 0 }))},
		{"no reference", mustJSON(t, mutate(func(e *Event) { e.Reference = "" }))},
		{"reference not under refs", mustJSON(t, mutate(func(e *Event) { e.Reference = "main" }))},
		{"reference with newline", mustJSON(t, mutate(func(e *Event) { e.Reference = "refs/heads/a\nb" }))},
		{"short new oid", mustJSON(t, mutate(func(e *Event) { e.NewOID = "abc" }))},
		{"uppercase new oid", mustJSON(t, mutate(func(e *Event) { e.NewOID = strings.Repeat("A", 40) }))},
		{"bad old oid", mustJSON(t, mutate(func(e *Event) { e.OldOID = "zz" }))},
		{"no recorded_at", mustJSON(t, mutate(func(e *Event) { e.RecordedAt = time.Time{} }))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseEvent(tc.line); err == nil {
				t.Fatalf("ParseEvent accepted %s", tc.line)
			}
		})
	}
}

func TestParseEventAllowsEmptyOldOID(t *testing.T) {
	ev := mkEvent(1, "land", "a", base)
	ev.OldOID = ""
	if _, err := ParseEvent(mustJSON(t, ev)); err != nil {
		t.Fatalf("empty old_oid rejected: %v", err)
	}
}

func TestParseEventAcceptsSHA256(t *testing.T) {
	ev := mkEvent(1, "land", "a", base)
	ev.NewOID = strings.Repeat("c", 64)
	ev.OldOID = strings.Repeat("0", 64)
	if _, err := ParseEvent(mustJSON(t, ev)); err != nil {
		t.Fatalf("sha256 object IDs rejected: %v", err)
	}
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReadEventsSkipsBlankLinesAndStopsOnError(t *testing.T) {
	doc := eventsDoc(t, mkEvent(1, "land", "a", base), mkEvent(2, "land", "b", base)) + "\n   \n"
	var seqs []uint64
	err := ReadEvents(strings.NewReader(doc), func(ev Event) error {
		seqs = append(seqs, ev.Sequence)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Errorf("sequences = %v", seqs)
	}

	bad := eventsDoc(t, mkEvent(1, "land", "a", base)) + "{nope}\n"
	err = ReadEvents(strings.NewReader(bad), func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("malformed line error = %v, want line 2", err)
	}

	stop := errors.New("stop")
	err = ReadEvents(strings.NewReader(doc), func(Event) error { return stop })
	if !errors.Is(err, stop) {
		t.Errorf("callback error not returned: %v", err)
	}
}

func TestReadEventsTooLongLine(t *testing.T) {
	long := `{"sequence":1,"reference":"refs/heads/` + strings.Repeat("x", MaxEventLine) + `"}`
	err := ReadEvents(bytes.NewReader([]byte(long)), func(Event) error { return nil })
	if err == nil {
		t.Fatal("oversized line accepted")
	}
}

func TestReaderSourceEOFAndCancel(t *testing.T) {
	src := NewReaderSource(strings.NewReader(eventsDoc(t, mkEvent(1, "land", "a", base))), nil)
	if _, err := src.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("second Next = %v, want EOF", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src = NewReaderSource(strings.NewReader(eventsDoc(t, mkEvent(1, "land", "a", base))), nil)
	if _, err := src.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Next = %v", err)
	}
	if err := src.Close(); err != nil {
		t.Errorf("Close without closer: %v", err)
	}
}

func TestReaderSourceReportsLineNumber(t *testing.T) {
	src := NewReaderSource(strings.NewReader("\n"+eventsDoc(t, mkEvent(1, "land", "a", base))+`{"sequence":0}`+"\n"), nil)
	if _, err := src.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := src.Next(context.Background())
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error = %v, want line 3", err)
	}
}
