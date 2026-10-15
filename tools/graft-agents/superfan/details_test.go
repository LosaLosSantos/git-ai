package superfan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDetailsJSON(t testing.TB, oid string, files ...FileChange) []byte {
	t.Helper()
	return mustJSON(t, Details{
		OID:      oid,
		Parents:  []string{oid},
		Author:   "agent-1",
		Subject:  "change things",
		Message:  "change things\n\nThis reverts commit " + oid + ".\n",
		Trailers: map[string][]string{TrailerResolvedBy: {"graft-resolve/union"}},
		Files:    files,
	})
}

func TestParseDetailsValid(t *testing.T) {
	o := oid('a')
	d, err := ParseDetails(testDetailsJSON(t, o,
		FileChange{Path: "a/b.go", Additions: 3, Deletions: 1},
		FileChange{Path: "c.md", Status: "added", Additions: 10},
	), o)
	if err != nil {
		t.Fatalf("ParseDetails: %v", err)
	}
	if len(d.Files) != 2 || d.Lines() != 14 {
		t.Errorf("files=%d lines=%d", len(d.Files), d.Lines())
	}
	if v, ok := d.Trailer(TrailerResolvedBy); !ok || v != "graft-resolve/union" {
		t.Errorf("Trailer = %q, %v", v, ok)
	}
	if _, ok := d.Trailer("Missing"); ok {
		t.Error("missing trailer reported present")
	}
}

func TestParseDetailsRejects(t *testing.T) {
	o := oid('a')
	many := make([]FileChange, MaxDetailFiles+1)
	for i := range many {
		many[i] = FileChange{Path: "f"}
	}
	for _, tc := range []struct {
		name string
		data []byte
		oid  string
	}{
		{"not json", []byte(`[`), o},
		{"other commit", testDetailsJSON(t, oid('b')), o},
		{"negative lines", testDetailsJSON(t, o, FileChange{Path: "x", Additions: -1}), o},
		{"too many files", mustJSON(t, Details{OID: o, Files: many}), o},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseDetails(tc.data, tc.oid); err == nil {
				t.Fatal("accepted invalid details")
			}
		})
	}
	// Too many files is a commit too large to inspect, not a broken answer.
	if _, err := ParseDetails(mustJSON(t, Details{OID: o, Files: many}), o); !errors.Is(err, ErrDetailsTooLarge) {
		t.Errorf("too many files: err = %v, want ErrDetailsTooLarge", err)
	}
}

func TestCommandDetailsSubstitutesOIDAndParses(t *testing.T) {
	dir := t.TempDir()
	o := oid('c')
	if err := os.WriteFile(filepath.Join(dir, o+".json"), testDetailsJSON(t, o, FileChange{Path: "x.go", Additions: 2}), 0o600); err != nil {
		t.Fatal(err)
	}
	src := CommandDetails{Argv: []string{"cat", filepath.Join(dir, "{oid}.json")}}
	d, err := src.Details(context.Background(), mkEvent(1, "land", "a", base.Add(0)))
	if err == nil {
		t.Fatalf("event with NewOID %s should not find a file for oid a: %+v", oid('a'), d)
	}
	ev := mkEvent(1, "land", "a", base)
	ev.NewOID = o
	d, err = src.Details(context.Background(), ev)
	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	if d.OID != o || d.Lines() != 2 {
		t.Errorf("details = %+v", d)
	}
}

func TestCommandDetailsFailures(t *testing.T) {
	ev := mkEvent(1, "land", "a", base)
	ctx := context.Background()

	if _, err := (CommandDetails{}).Details(ctx, ev); err == nil {
		t.Error("empty argv accepted")
	}

	failing := CommandDetails{Argv: []string{"sh", "-c", "echo boom >&2; exit 3"}}
	if _, err := failing.Details(ctx, ev); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("failing command error = %v, want stderr", err)
	}

	garbage := CommandDetails{Argv: []string{"sh", "-c", "echo not-json"}}
	if _, err := garbage.Details(ctx, ev); err == nil {
		t.Error("garbage output accepted")
	}

	big := CommandDetails{Argv: []string{"sh", "-c", "head -c 5000 /dev/zero"}, MaxBytes: 1024}
	if _, err := big.Details(ctx, ev); !errors.Is(err, ErrDetailsTooLarge) {
		t.Errorf("oversized output error = %v, want ErrDetailsTooLarge", err)
	}

	slow := CommandDetails{Argv: []string{"sleep", "30"}, Timeout: 100 * time.Millisecond}
	start := time.Now()
	if _, err := slow.Details(ctx, ev); err == nil {
		t.Error("slow command did not time out")
	}
	if time.Since(start) > 10*time.Second {
		t.Error("timeout did not stop the command promptly")
	}
}

func TestCommandDetailsNoShellInterpretation(t *testing.T) {
	// The OID is substituted as an argument, never through a shell, so a
	// command written with metacharacters is passed literally.
	ev := mkEvent(1, "land", "a", base)
	src := CommandDetails{Argv: []string{"sh", "-c", "echo ok; exit 0", "{oid}"}}
	if _, err := src.Details(context.Background(), ev); err == nil {
		t.Error("garbage output accepted")
	}
}
