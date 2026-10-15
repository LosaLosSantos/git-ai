package resolve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/open_scion/tools/graft-agents/protocol"
)

func newTestRerere(t *testing.T, dir string) rerere {
	t.Helper()
	s, err := newRerere(StrategySpec{Name: "rr", Type: "rerere", Options: map[string]any{"dir": dir}})
	if err != nil {
		t.Fatalf("newRerere: %v", err)
	}
	return s.(rerere)
}

func TestRerereRecordThenLookup(t *testing.T) {
	dir := t.TempDir()
	s := newTestRerere(t, dir)
	f := conflict("a.go")
	ctx := context.Background()

	if _, ok, err := s.Resolve(ctx, f, protocol.Request{}); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}

	want := Resolution{Action: protocol.ActionWrite, Mode: protocol.ModeExecutable, Content: []byte("resolved\n")}
	if err := s.Record(ctx, f, want); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// The same three contents under another path hit the same entry.
	other := f
	other.Path = "elsewhere/b.go"
	got, ok, err := s.Resolve(ctx, other, protocol.Request{})
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.Action != want.Action || got.Mode != want.Mode || string(got.Content) != string(want.Content) {
		t.Fatalf("lookup = %+v, want %+v", got, want)
	}
}

func TestRerereDeleteAnswer(t *testing.T) {
	s := newTestRerere(t, t.TempDir())
	f := conflict("gone.go")
	ctx := context.Background()
	if err := s.Record(ctx, f, Resolution{Action: protocol.ActionDelete}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Resolve(ctx, f, protocol.Request{})
	if err != nil || !ok || got.Action != protocol.ActionDelete || got.Mode != "" || got.Content != nil {
		t.Fatalf("delete lookup = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestRerereKeySensitivity(t *testing.T) {
	base := conflict("a.go")
	k := rerereKey(base)
	if len(k) != 64 || strings.Trim(k, "0123456789abcdef") != "" {
		t.Fatalf("key %q is not 64 hex digits", k)
	}
	if rerereKey(base) != k {
		t.Fatal("key not stable")
	}
	variants := map[string]File{
		"ours content": func() File { f := base; f.Ours = sideOf(protocol.ModeFile, "other\n"); return f }(),
		"theirs mode":  func() File { f := base; f.Theirs = sideOf(protocol.ModeExecutable, "theirs\n"); return f }(),
		"base absent":  func() File { f := base; f.Base = nil; return f }(),
		"ours absent":  func() File { f := base; f.Ours = nil; return f }(),
	}
	for name, v := range variants {
		if rerereKey(v) == k {
			t.Errorf("%s does not change the key", name)
		}
	}
	// Length prefixing: moving bytes between sides must change the key.
	a := conflict("x")
	a.Base = sideOf(protocol.ModeFile, "ab")
	a.Ours = sideOf(protocol.ModeFile, "c")
	b := conflict("x")
	b.Base = sideOf(protocol.ModeFile, "a")
	b.Ours = sideOf(protocol.ModeFile, "bc")
	if rerereKey(a) == rerereKey(b) {
		t.Fatal("ambiguous encoding of sides")
	}
}

func TestRerereRejectsCorruptEntries(t *testing.T) {
	dir := t.TempDir()
	s := newTestRerere(t, dir)
	f := conflict("a.go")
	path := filepath.Join(dir, rerereKey(f)+".json")
	ctx := context.Background()

	cases := map[string]string{
		"not json":     `{not json`,
		"bad action":   `{"action":"explode","content":null}`,
		"bad mode":     `{"action":"write","mode":"777","content":"eA=="}`,
		"missing mode": `{"action":"write","content":"eA=="}`,
		"bad base64":   `{"action":"write","mode":"100644","content":"%%%"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := s.Resolve(ctx, f, protocol.Request{}); err == nil || ok {
				t.Fatalf("corrupt entry accepted: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestRerereUnreadableEntry(t *testing.T) {
	dir := t.TempDir()
	s := newTestRerere(t, dir)
	f := conflict("a.go")
	// A directory where the entry should be makes ReadFile fail with a
	// non-"not exist" error.
	if err := os.Mkdir(filepath.Join(dir, rerereKey(f)+".json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Resolve(context.Background(), f, protocol.Request{}); err == nil || ok {
		t.Fatalf("unreadable entry: ok=%v err=%v", ok, err)
	}
}

func TestRerereRecordLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := newTestRerere(t, dir)
	if err := s.Record(context.Background(), conflict("a.go"), Resolution{Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("store contents = %v", entries)
	}
}

func TestRerereConcurrentRecord(t *testing.T) {
	dir := t.TempDir()
	s := newTestRerere(t, dir)
	f := conflict("a.go")
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Record(context.Background(), f, Resolution{Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: []byte("same\n")})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Record: %v", err)
		}
	}
	got, ok, err := s.Resolve(context.Background(), f, protocol.Request{})
	if err != nil || !ok || string(got.Content) != "same\n" {
		t.Fatalf("after concurrent writes: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestRerereFactoryValidation(t *testing.T) {
	if _, err := newRerere(StrategySpec{Name: "rr", Type: "rerere"}); err == nil {
		t.Fatal("rerere without dir accepted")
	}
	if _, err := newRerere(StrategySpec{Name: "rr", Type: "rerere", Options: map[string]any{"dir": "x", "other": 1}}); err == nil {
		t.Fatal("unknown option accepted")
	}
}
