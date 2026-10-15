package graft

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAcceptancePhase4DryRunChecksP99Presence is the regression test for acceptance-phase4.sh,
// which reported PASS for the p99 criterion when p99_ms was missing from the results. The
// criterion needs both p99 values, so a missing one must FAIL and exit non-zero. The test runs the
// --dry-run path of the script on fixtures, and needs bash and python3.
func TestAcceptancePhase4DryRunChecksP99Presence(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not available")
	}

	script, err := filepath.Abs(filepath.Join("..", "..", "examples", "graft", "demo", "acceptance-phase4.sh"))
	if err != nil {
		t.Fatal(err)
	}

	// writeRun writes the results of one run; the on run is the one under test.
	writeRun := func(t *testing.T, onP99 *float64) string {
		t.Helper()
		work := t.TempDir()
		results := filepath.Join(work, "results")
		if err := os.MkdirAll(results, 0o755); err != nil {
			t.Fatal(err)
		}

		off := map[string]any{"landed": 80, "attempts_per_land": 5.0, "failed": 0, "conflicts": 0, "p99_ms": 5000.0}
		on := map[string]any{"landed": 80, "attempts_per_land": 1.0, "failed": 20, "conflicts": 20}
		if onP99 != nil {
			on["p99_ms"] = *onP99
		}

		for name, run := range map[string]map[string]any{"off-50.json": off, "on-50.json": on} {
			data, err := json.Marshal(run)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(results, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return work
	}

	runScript := func(t *testing.T, work string) (string, error) {
		t.Helper()
		cmd := exec.Command(bash, script, "--dry-run", work)
		cmd.Env = append(os.Environ(), "AGENT_COUNTS=50")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("missing p99 fails", func(t *testing.T) {
		out, err := runScript(t, writeRun(t, nil))
		if err == nil {
			t.Fatalf("the script must exit non-zero when p99_ms is missing, output:\n%s", out)
		}
		if !strings.Contains(out, "FAIL p99") {
			t.Errorf("the script must print a FAIL line for the p99 criterion, output:\n%s", out)
		}
		if strings.Contains(out, "PASS p99") {
			t.Errorf("the script must not print PASS for the p99 criterion, output:\n%s", out)
		}
	})

	t.Run("present p99 passes", func(t *testing.T) {
		p99 := 100.0
		out, err := runScript(t, writeRun(t, &p99))
		if err != nil {
			t.Fatalf("the script must pass on healthy results: %v, output:\n%s", err, out)
		}
		if !strings.Contains(out, "PASS p99 on 100.0 ms <= off 5000.0 ms, 50 agents") {
			t.Errorf("unexpected p99 line, output:\n%s", out)
		}
	})
}
