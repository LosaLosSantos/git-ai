package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/tools/graft-agents/notify"
)

func oid(c byte) string { return strings.Repeat(string(c), 40) }

// event returns one JSON line of graft watch --json.
func event(seq uint64, c byte, actor string, minute int) string {
	recorded := time.Date(2026, 10, 1, 10, minute, 0, 0, time.UTC)
	return fmt.Sprintf(`{"sequence":%d,"reference":"refs/heads/main","old_oid":"%s","new_oid":"%s","operation":"land","actor":%q,"recorded_at":%q,"rewritten":false}`+"\n",
		seq, oid('0'), oid(c), actor, recorded.Format(time.RFC3339))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stdoutConfig returns a configuration that prints digests and flags actors
// outside "ci".
func stdoutConfig(cursor string) string {
	c := `{
  "window": "1h",
  "notifiers": [{"name": "console", "type": "stdout"}],
  "routes": [{"component": "*", "notifiers": ["console"]}],
  "rules": {"expected_actors": ["ci"]}`
	if cursor != "" {
		c += fmt.Sprintf(`, "cursor": %q`, cursor)
	}
	return c + "}"
}

func TestRunUsageErrors(t *testing.T) {
	ctx := context.Background()
	for _, args := range [][]string{
		{},
		{"--config"},
		{"--bogus-flag"},
		{"--config", "x.json", "extra"},
		{"--config", "x.json", "--tick", "-1s"},
	} {
		var stderr bytes.Buffer
		if code := run(ctx, args, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != exitUsage {
			t.Errorf("run(%q) = %d, want %d (stderr %q)", args, code, exitUsage, stderr.String())
		}
	}
	var stderr bytes.Buffer
	if code := run(ctx, []string{"-h"}, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != exitOK {
		t.Errorf("-h = %d, want 0", code)
	}
}

func TestRunConfigErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	writeFile(t, bad, `{"windoww": "1h"}`)
	for _, cfgPath := range []string{filepath.Join(dir, "absent.json"), bad} {
		var stderr bytes.Buffer
		code := run(context.Background(), []string{"--config", cfgPath, "--tick", "0"}, strings.NewReader(""), &bytes.Buffer{}, &stderr)
		if code != exitError || stderr.Len() == 0 {
			t.Errorf("config %s: code %d, stderr %q", cfgPath, code, stderr.String())
		}
	}
}

func TestRunStdinDigestAndCursor(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "superfan.json")
	cursorPath := filepath.Join(dir, "cursor.json")
	writeFile(t, cfgPath, stdoutConfig(cursorPath))

	input := event(1, 'a', "ci", 1) + event(2, 'b', "mallory", 2) + event(3, 'c', "mallory", 3)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--config", cfgPath, "--tick", "0"}, strings.NewReader(input), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("run = %d, stderr %q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "[digest] component=unassigned") || !strings.Contains(out, `actor "mallory" is not expected`) {
		t.Errorf("digest missing:\n%s", out)
	}
	if strings.Count(out, "- [high/unexpected-actor]") != 2 {
		t.Errorf("want 2 findings:\n%s", out)
	}
	data, err := os.ReadFile(cursorPath)
	if err != nil || !strings.Contains(string(data), `"sequence":3`) {
		t.Errorf("cursor file = %q, %v", data, err)
	}

	// The next run resumes after the cursor and so delivers only new events.
	stdout.Reset()
	input = event(4, 'd', "mallory", 4)
	if code := run(context.Background(), []string{"--config", cfgPath, "--tick", "0"}, strings.NewReader(input), &stdout, &stderr); code != exitOK {
		t.Fatalf("second run = %d, stderr %q", code, stderr.String())
	}
	if strings.Count(stdout.String(), "- [high/unexpected-actor]") != 1 || !strings.Contains(stdout.String(), "seq 4") {
		t.Errorf("second run output:\n%s", stdout.String())
	}
}

func TestRunEventsFileAndDetailsCommand(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "superfan.json")
	writeFile(t, cfgPath, `{
  "window": "1h",
  "components": [{"name": "payments", "paths": ["ledger/**"]}],
  "notifiers": [{"name": "console", "type": "stdout"}],
  "routes": [{"component": "payments", "notifiers": ["console"]}],
  "rules": {"protected_paths": ["ledger/**"]}
}`)
	eventsPath := filepath.Join(dir, "events.jsonl")
	writeFile(t, eventsPath, event(1, 'e', "agent-1", 1))
	details := fmt.Sprintf(`{"oid":%q,"parents":[],"author":"agent-1","subject":"x","message":"x","trailers":{},"files":[{"path":"ledger/a.go","additions":2,"deletions":0}]}`, oid('e'))
	writeFile(t, filepath.Join(dir, oid('e')+".json"), details)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--config", cfgPath,
		"--events", eventsPath,
		"--details-cmd", "cat " + filepath.Join(dir, "{oid}.json"),
		"--tick", "0",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("run = %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "component=payments") || !strings.Contains(stdout.String(), "protected-path") {
		t.Errorf("payments digest missing:\n%s", stdout.String())
	}
}

func TestRunMissingEventsFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "superfan.json")
	writeFile(t, cfgPath, stdoutConfig(""))
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"--config", cfgPath, "--events", filepath.Join(dir, "nope")}, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "open events") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
}

func TestRunWatchCommandSubstitutesAfter(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "superfan.json")
	cursorPath := filepath.Join(dir, "cursor.json")
	writeFile(t, cfgPath, stdoutConfig(cursorPath))
	// The watch command prints the file named after the cursor, so the file
	// named events-0 is read first, and the cursor then becomes 2.
	writeFile(t, filepath.Join(dir, "events-0"), event(1, 'a', "mallory", 1)+event(2, 'b', "mallory", 2))
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--config", cfgPath,
		"--watch-cmd", "cat " + filepath.Join(dir, "events-{after}"),
		"--tick", "0",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("run = %d, stderr %q", code, stderr.String())
	}
	if strings.Count(stdout.String(), "- [high/unexpected-actor]") != 2 {
		t.Errorf("watch output not delivered:\n%s", stdout.String())
	}
	if data, _ := os.ReadFile(cursorPath); !strings.Contains(string(data), `"sequence":2`) {
		t.Errorf("cursor = %q", data)
	}
}

func TestRunWatchCommandFailureIsAnError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "superfan.json")
	writeFile(t, cfgPath, stdoutConfig(""))
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"--config", cfgPath, "--watch-cmd", "/nonexistent/graft-xyz", "--tick", "0"},
		strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if code != exitError {
		t.Errorf("missing watch command: code %d, stderr %q", code, stderr.String())
	}
}

func TestRunWebhookReceivesDigest(t *testing.T) {
	var mu sync.Mutex
	var got []notify.Message
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var msg notify.Message
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		got = append(got, msg)
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		rw.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	writeFile(t, tokenPath, "hook-token\n")
	cfgPath := filepath.Join(dir, "superfan.json")
	writeFile(t, cfgPath, fmt.Sprintf(`{
  "notifiers": [{"name": "audit", "type": "webhook", "url": %q, "token_file": %q}],
  "routes": [{"component": "*", "notifiers": ["audit"]}],
  "rules": {"expected_actors": ["ci"]}
}`, srv.URL, tokenPath))

	events := filepath.Join(dir, "events.jsonl")
	writeFile(t, events, event(1, 'a', "mallory", 1))
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--config", cfgPath, "--events", events, "--tick", "0"},
		strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("run = %d, stderr %q", code, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Component != "unassigned" || len(got[0].Findings) != 1 || got[0].Findings[0].Rule != "unexpected-actor" {
		t.Fatalf("webhook got %+v", got)
	}
	if auth != "Bearer hook-token" {
		t.Errorf("Authorization = %q", auth)
	}
	if stdout.Len() != 0 {
		t.Errorf("webhook-only config printed to stdout: %q", stdout.String())
	}
}

func TestRunWebhookFailureExitsNonZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "superfan.json")
	writeFile(t, cfgPath, fmt.Sprintf(`{
  "notifiers": [{"name": "audit", "type": "webhook", "url": %q}],
  "routes": [{"component": "*", "notifiers": ["audit"]}],
  "rules": {"expected_actors": ["ci"]}
}`, srv.URL))
	events := filepath.Join(dir, "events.jsonl")
	writeFile(t, events, event(1, 'a', "mallory", 1))
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"--config", cfgPath, "--events", events, "--tick", "0"},
		strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "500") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), srv.URL) {
		t.Errorf("stderr echoes the webhook URL: %q", stderr.String())
	}
}

func TestRunCancelledIsCleanStop(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "superfan.json")
	writeFile(t, cfgPath, stdoutConfig(""))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--config", cfgPath, "--watch-cmd", "sleep 30", "--tick", "0"},
			strings.NewReader(""), &bytes.Buffer{}, &stderr)
	}()
	select {
	case code := <-done:
		if code != exitOK {
			t.Errorf("interrupted run = %d", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("interrupt did not stop the watch command")
	}
}
