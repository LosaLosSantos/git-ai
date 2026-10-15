package main

// Black-box spec tests for WP48 (cache snapshot seeding). The spec names no
// exported Go identifiers for this work package, so these tests drive the
// graft-cache binary through its command line only. They were written without
// access to the implementation and have not been compiled against it.

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func buildGraftCacheSpec(t *testing.T) string {
	t.Helper()

	name := "graft-cache"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)

	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building graft-cache failed: %v\n%s", err, out)
	}
	return bin
}

// runGraftCacheSpec runs the binary and returns its combined output and exit
// code. A timeout protects against a binary that waits on a dead upstream.
func runGraftCacheSpec(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	if err == nil {
		return buf.String(), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return buf.String(), ee.ExitCode()
	}
	t.Fatalf("running graft-cache %v: %v", args, err)
	return "", -1
}

// TestSpecSnapshotRequiresOut checks that snapshot refuses to run without the
// --out FILE destination named in the spec.
func TestSpecSnapshotRequiresOut(t *testing.T) {
	bin := buildGraftCacheSpec(t)

	out, code := runGraftCacheSpec(t, bin,
		"snapshot",
		"--upstream", "127.0.0.1:1",
		"--token", "t",
		"--storage", "default",
		"--repo", "example.git",
		"--rev", "HEAD",
	)
	if code == 0 {
		t.Fatalf("snapshot without --out exited 0, output:\n%s", out)
	}
}

// TestSpecSnapshotUnreachableUpstreamFails checks that a snapshot cannot
// succeed when the upstream gitaly is not listening. Nothing can be produced
// from an upstream that does not answer GetTree, so exit status must be non-zero.
func TestSpecSnapshotUnreachableUpstreamFails(t *testing.T) {
	bin := buildGraftCacheSpec(t)
	outFile := filepath.Join(t.TempDir(), "seed.gz")

	out, code := runGraftCacheSpec(t, bin,
		"snapshot",
		"--upstream", "127.0.0.1:1",
		"--token", "t",
		"--storage", "default",
		"--repo", "example.git",
		"--rev", "HEAD",
		"--out", outFile,
	)
	if code == 0 {
		t.Fatalf("snapshot against a dead upstream exited 0, output:\n%s", out)
	}
}

// TestSpecSeedFlagDocumented checks that the top level help advertises the
// --seed FILE[,FILE] preload option named in the spec.
func TestSpecSeedFlagDocumented(t *testing.T) {
	bin := buildGraftCacheSpec(t)

	out, _ := runGraftCacheSpec(t, bin, "--help")
	if !strings.Contains(strings.ToLower(out), "seed") {
		t.Fatalf("top level help does not mention --seed; output:\n%s", out)
	}
}
