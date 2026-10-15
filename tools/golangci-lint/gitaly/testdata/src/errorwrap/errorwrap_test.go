package errorwrap

import (
	"fmt"
)

// This file is the test fixture for Gitaly linters

func call(format string, err error) {}

func errorWrapOkay() {
	err := fmt.Errorf("test error")

	_ = fmt.Errorf("error: %s", "something else")
	_ = fmt.Errorf("error: %v", "something else")
	_ = fmt.Errorf("error: %q", "something else")
	_ = fmt.Errorf("error: %s %d", "something else", 5)
	_ = fmt.Errorf("error: %w", err)
	_ = fmt.Errorf("error: %w", fmt.Errorf("error: %s", "hello"))

	call("error: %w", fmt.Errorf("error: %s", "hello"))
	_ = fmt.Sprintf("error: %s", err)
}

func errorWrapNotOkay() {
	err := fmt.Errorf("test error")

	_ = fmt.Errorf("error: %s", err)                      // want "please use %w to wrap errors"
	_ = fmt.Errorf("error: %s", fmt.Errorf("test error")) // want "please use %w to wrap errors"
	_ = fmt.Errorf("error: %v", err)                      // want "please use %w to wrap errors"
	_ = fmt.Errorf("error: %v", fmt.Errorf("test error")) // want "please use %w to wrap errors"
	_ = fmt.Errorf("error: %q", err)                      // want "please use %w to wrap errors"
	_ = fmt.Errorf("error: %q", fmt.Errorf("test error")) // want "please use %w to wrap errors"
	_ = fmt.Errorf("error number %d: %s", 5, err)         // want "please use %w to wrap errors"
	_ = fmt.Errorf("error: %w", err)
	_ = fmt.Errorf("error: %w", fmt.Errorf("error: %s", err)) // want "please use %w to wrap errors"
}
