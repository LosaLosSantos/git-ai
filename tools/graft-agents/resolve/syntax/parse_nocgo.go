//go:build !cgo

package syntax

import "context"

// Available reports whether this build parses (it was built with cgo).
const Available = false

func parse(context.Context, string, string, []byte) error {
	return ErrUnavailable
}
