package main

// Black-box spec tests for WP28 (graft-bench v2). Written from PLAN3.md only.
// They run from the package directory (cmd/graft-bench) once merged into the
// repository. The spec names no exported Go identifiers for percentile math or
// report formatting, so those are covered only through the built binary and
// the demo script.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const specBenchScript = "../../examples/graft/demo/bench-batch.sh"

// specBuildBench builds the graft-bench binary from the package directory.
func specBuildBench(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "graft-bench")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build of cmd/graft-bench failed: %v\n%s", err, out)
	}

	return bin
}

// specReadScript reads examples/graft/demo/bench-batch.sh and fails if absent.
func specReadScript(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(specBenchScript)
	if err != nil {
		t.Fatalf("WP28 requires %s to exist: %v", specBenchScript, err)
	}

	return string(data)
}

// TestSpecFlagsDocumented checks that the WP28 flags are part of the CLI.
// The spec adds --branches, --label, --json-out and --md-out. Usage output is
// requested with -h; the exit code is not asserted.
func TestSpecFlagsDocumented(t *testing.T) {
	bin := specBuildBench(t)

	out, _ := exec.Command(bin, "-h").CombinedOutput()
	usage := string(out)
	if strings.TrimSpace(usage) == "" {
		t.Fatalf("graft-bench -h printed nothing; expected usage listing the flags")
	}

	for _, flag := range []string{"branches", "label", "json-out", "md-out"} {
		if !strings.Contains(usage, flag) {
			t.Errorf("usage output does not mention flag %q (WP28 requires it)\nusage:\n%s", flag, usage)
		}
	}
}

// TestSpecBenchBatchScriptExists checks that the demo script exists and has
// the required shape: it builds binaries and cleans up on EXIT via trap.
func TestSpecBenchBatchScriptExists(t *testing.T) {
	script := specReadScript(t)

	if !strings.HasPrefix(script, "#!") {
		t.Errorf("bench-batch.sh should start with a shebang line")
	}

	if !strings.Contains(script, "go build") {
		t.Errorf("bench-batch.sh must build the binaries (expected a 'go build' invocation)")
	}

	trapExit := regexp.MustCompile(`trap\s+.*\bEXIT\b`)
	if !trapExit.MatchString(script) {
		t.Errorf("bench-batch.sh must kill both servers via 'trap ... EXIT'")
	}
}
