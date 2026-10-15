package errorwrap

import (
	f "fmt"
)

// This file is the test fixture for Gitaly linters

func errorWrapAliasOkay() {
	err := f.Errorf("test error")

	_ = f.Errorf("error: %s", "something else")
	_ = f.Errorf("error: %v", "something else")
	_ = f.Errorf("error: %q", "something else")
	_ = f.Errorf("error: %s %d", "something else", 5)
	_ = f.Errorf("error: %w", err)
	_ = f.Errorf("error: %w", f.Errorf("error: %s", "hello"))
}

func errorWrapAliasNotOkay() {
	err := f.Errorf("test error")

	_ = f.Errorf("error: %s", err)                    // want "please use %w to wrap errors"
	_ = f.Errorf("error: %s", f.Errorf("test error")) // want "please use %w to wrap errors"
	_ = f.Errorf("error: %v", err)                    // want "please use %w to wrap errors"
	_ = f.Errorf("error: %v", f.Errorf("test error")) // want "please use %w to wrap errors"
	_ = f.Errorf("error: %q", err)                    // want "please use %w to wrap errors"
	_ = f.Errorf("error: %q", f.Errorf("test error")) // want "please use %w to wrap errors"
	_ = f.Errorf("error number %d: %s", 5, err)       // want "please use %w to wrap errors"
	_ = f.Errorf("error: %w", err)
	_ = f.Errorf("error: %w", f.Errorf("error: %s", err)) // want "please use %w to wrap errors"
}
