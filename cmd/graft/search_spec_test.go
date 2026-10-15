package main

// Black-box spec tests for WP42 (the `graft search` CLI command). Written from
// doc/graft/PLAN5.md only. The binary is built and run as a subprocess so the
// tests depend only on the documented command line, not on internal functions.
// Not compiled against the implementation.

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// specBuildGraft builds the graft binary into a temp dir and returns its path.
func specBuildGraft(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "graft")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "github.com/feder-cr/open_scion/cmd/graft")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building graft: %v\n%s", err, out)
	}
	return bin
}

// TestSpecSearchCLIRequiresQuery checks that `graft search` without a QUERY
// argument fails with a non-zero exit status.
func TestSpecSearchCLIRequiresQuery(t *testing.T) {
	bin := specBuildGraft(t)

	cmd := exec.Command(bin, "search")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("graft search without QUERY: expected non-zero exit, output:\n%s", out)
	}
}
