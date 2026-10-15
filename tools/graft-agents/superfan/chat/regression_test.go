package chat

import (
	"strings"
	"testing"
	"time"
)

// builtinRedactor returns a redactor with only the built-in patterns.
func builtinRedactor(t *testing.T) *redactor {
	t.Helper()
	r, err := newRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestPreviewRedactsBeforeTruncating checks that a secret cut by the summary
// width does not leave a partial prefix behind: the item is redacted first.
func TestPreviewRedactsBeforeTruncating(t *testing.T) {
	r := builtinRedactor(t)
	// The AWS key starts at byte 141 and would be cut after "AKIAABC" by the
	// summary width, which no longer matches the key pattern.
	item := strings.Repeat("x", 140) + " AKIAABCDEFGHIJKLMNOP"
	out := previewItems([]string{item}, r)
	if strings.Contains(out, "AKIA") {
		t.Fatalf("preview leaked a key prefix: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("preview did not redact the key: %q", out)
	}
}

// TestDrainWaitsForAnswersWithinBound checks that Drain waits for a running
// answer, gives up after its timeout, and reports completion afterwards.
func TestDrainWaitsForAnswersWithinBound(t *testing.T) {
	s := &Server{}
	s.tasks.Add(1)
	if s.Drain(10 * time.Millisecond) {
		t.Fatal("Drain reported completion while an answer was still running")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		s.tasks.Done()
	}()
	if !s.Drain(5 * time.Second) {
		t.Fatal("Drain timed out after the answer finished")
	}
}

// TestDrainWithoutAnswers checks that Drain returns at once when nothing runs.
func TestDrainWithoutAnswers(t *testing.T) {
	s := &Server{}
	if !s.Drain(time.Second) {
		t.Fatal("Drain did not return with no answers in progress")
	}
}
