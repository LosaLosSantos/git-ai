package notify

import (
	"context"
	"io"
	"sync"
)

// Stdout writes digests as plain text to a writer (normally os.Stdout).
type Stdout struct {
	name string
	mu   sync.Mutex
	w    io.Writer
}

// NewStdout returns a notifier that writes to w.
func NewStdout(name string, w io.Writer) *Stdout {
	return &Stdout{name: name, w: w}
}

// Name returns the configured name.
func (s *Stdout) Name() string { return s.name }

// Notify writes the text rendering of msg.
func (s *Stdout) Notify(ctx context.Context, msg Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := io.WriteString(s.w, Text(msg))
	return err
}
