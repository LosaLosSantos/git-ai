package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsEventsWithWatchCommand(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "superfan.json")
	writeFile(t, cfg, stdoutConfig(""))

	var stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"--config", cfg, "--events", "events.jsonl", "--watch-cmd", "true"},
		strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if code != exitUsage || !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("exit = %d, stderr = %q; want a usage error", code, stderr.String())
	}
}
