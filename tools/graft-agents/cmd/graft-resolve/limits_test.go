package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestCommandMaxRequestBytesFlag(t *testing.T) {
	var stderr bytes.Buffer
	body := strings.NewReader(`{"version": 1, "files": []}`)
	if code := run([]string{"--max-request-bytes", "10"}, body, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "request too large") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCommandMaxRequestBytesMustBePositive(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"--max-request-bytes", "0"}, strings.NewReader("{}"), io.Discard, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "must be positive") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
