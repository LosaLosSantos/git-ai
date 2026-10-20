//go:build !cgo

package syntax

import (
	"context"
	"errors"
	"testing"
)

// TestCheckFailsClosedWithoutCgo checks that a build without the grammars rejects the content of
// every path that has one, and leaves the other paths alone.
func TestCheckFailsClosedWithoutCgo(t *testing.T) {
	if err := Check(context.Background(), "a.go", []byte("package a\n")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if err := Check(context.Background(), "README.md", []byte("# a\n")); err != nil {
		t.Fatalf("path without a grammar: %v", err)
	}
}
