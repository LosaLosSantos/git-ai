package superfan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorMissingIsZero(t *testing.T) {
	seq, err := NewCursorStore(filepath.Join(t.TempDir(), "absent")).Load()
	if err != nil || seq != 0 {
		t.Fatalf("Load = %d, %v; want 0, nil", seq, err)
	}
}

func TestCursorSaveLoadOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "superfan.cursor")
	store := NewCursorStore(path)
	for _, seq := range []uint64{5, 42, 1 << 40} {
		if err := store.Save(seq); err != nil {
			t.Fatalf("Save(%d): %v", seq, err)
		}
		got, err := store.Load()
		if err != nil || got != seq {
			t.Fatalf("Load after Save(%d) = %d, %v", seq, got, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("temporary files left behind: %v", names)
	}
}

func TestCursorCorruptAndOversized(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCursorStore(corrupt).Load(); err == nil {
		t.Error("corrupt cursor accepted")
	}

	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(`{"sequence":1,"pad":"`+strings.Repeat("x", maxCursorFile)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCursorStore(big).Load(); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("oversized cursor: err = %v", err)
	}
}

func TestCursorSaveFailsInMissingDirectory(t *testing.T) {
	store := NewCursorStore(filepath.Join(t.TempDir(), "no", "such", "dir", "cursor"))
	if err := store.Save(1); err == nil {
		t.Fatal("Save into a missing directory succeeded")
	}
}

func TestReadAllLimitedBoundary(t *testing.T) {
	data, err := readAllLimited(strings.NewReader("abcd"), 4)
	if err != nil || string(data) != "abcd" {
		t.Fatalf("exact limit: %q, %v", data, err)
	}
	if _, err := readAllLimited(strings.NewReader("abcde"), 4); err == nil {
		t.Fatal("one byte over the limit accepted")
	}
}
