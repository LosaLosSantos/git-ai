package superfan

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandSourceReadsEventsThenEOF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(eventsDoc(t,
		mkEvent(1, "land", "a", base),
		mkEvent(2, "land", "b", base.Add(time.Second)),
	)), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := StartCommand(context.Background(), []string{"cat", path})
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	defer src.Close()

	for want := uint64(1); want <= 2; want++ {
		ev, err := src.Next(context.Background())
		if err != nil {
			t.Fatalf("Next %d: %v", want, err)
		}
		if ev.Sequence != want {
			t.Errorf("sequence = %d, want %d", ev.Sequence, want)
		}
	}
	if _, err := src.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("after the last event: %v, want EOF", err)
	}
}

func TestCommandSourceFailureCarriesStderr(t *testing.T) {
	src, err := StartCommand(context.Background(), []string{"sh", "-c", "echo no repository >&2; exit 4"})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	_, err = src.Next(context.Background())
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("failing command: err = %v", err)
	}
	if !strings.Contains(err.Error(), "no repository") {
		t.Errorf("stderr not reported: %v", err)
	}
}

func TestCommandSourceCancelKillsCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src, err := StartCommand(ctx, []string{"sleep", "60"})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	done := make(chan error, 1)
	go func() {
		_, err := src.Next(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Next returned no error after cancellation")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not stop the command")
	}
}

func TestCommandSourceCloseKillsRunningCommand(t *testing.T) {
	src, err := StartCommand(context.Background(), []string{"sleep", "60"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := src.Next(context.Background())
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Next returned no error after Close")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not stop the command")
	}
}

func TestStartCommandErrors(t *testing.T) {
	if _, err := StartCommand(context.Background(), nil); err == nil {
		t.Error("empty argv accepted")
	}
	if _, err := StartCommand(context.Background(), []string{"/nonexistent/graft-binary-xyz"}); err == nil {
		t.Error("missing binary accepted")
	}
}

func TestCappedBufferKeepsPrefix(t *testing.T) {
	c := &cappedBuffer{max: 5}
	for _, p := range []string{"abc", "defgh", "ij"} {
		n, err := c.Write([]byte(p))
		if n != len(p) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", p, n, err)
		}
	}
	if got := c.buf.String(); got != "abcde" {
		t.Errorf("kept %q, want abcde", got)
	}
}
