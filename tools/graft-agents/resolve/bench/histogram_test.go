//go:build cgo

package bench

import (
	"context"
	"testing"
)

// TestPatchDiffsAgreeOnASimpleConflict: on a one-line conflict our PATCH and git's histogram diff
// are the same text.
func TestPatchDiffsAgreeOnASimpleConflict(t *testing.T) {
	f := newFixture(t)
	buildHistory(f)
	head := f.git("rev-parse", "HEAD")
	cases, _, err := Mine(context.Background(), Repo{Name: "fixture", Commit: head, License: "MIT"}, f.dir, MineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := PatchDiffs(context.Background(), cases, func(string) string { return f.dir }, 3)
	if err != nil {
		t.Fatal(err)
	}
	s := stats["go"]
	if s.Cases != 1 || s.Regions != 1 || s.DifferentRegion != 0 || s.DifferentCases != 0 {
		t.Fatalf("stats %+v, want one region that agrees", stats)
	}
}
