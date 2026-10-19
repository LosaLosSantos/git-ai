package resolver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// maxStderrBytes bounds the stderr kept as the refusal reason.
	maxStderrBytes = 4 << 10
	// maxErrorBodyBytes bounds the body of an HTTP 422 kept as the reason.
	maxErrorBodyBytes = 4 << 10
	// commandWaitDelay bounds how long a killed resolver may keep its output
	// pipes open (for example through a grandchild process) before they are
	// closed forcibly.
	commandWaitDelay = 2 * time.Second
)

// errOutputExceeded stops the copy of an over-long stdout.
var errOutputExceeded = errors.New("resolver output exceeds its bound")

// callCommand runs the resolver command with body on stdin and returns its
// stdout. Stdout is bounded by limit. A non-zero exit is an
// UnresolvableError whose reason is the stderr. The command gets a minimal
// environment, so operator secrets in the server's environment are not
// exposed to it.
func (c *Client) callCommand(ctx context.Context, body []byte, limit int64) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.command[0], c.command[1:]...)
	cmd.Env = resolverEnv()
	cmd.Stdin = bytes.NewReader(body)
	cmd.WaitDelay = commandWaitDelay

	stdout := &boundedBuffer{limit: limit, stop: cancel}
	stderr := &boundedBuffer{limit: maxStderrBytes, discard: true}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

	if stdout.exceeded {
		return nil, fmt.Errorf("%w: output exceeds %d bytes", ErrInvalidResponse, limit)
	}

	if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
		// The context ended the run (the caller's deadline or cancellation);
		// the process was killed, so the exit status says nothing about the
		// conflict.
		return nil, fmt.Errorf("run resolver: %w", ctxErr)
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// A refusal may carry an ErrorResponse on stdout, with the outcome of
			// every file; otherwise the stderr is the reason.
			if refusal, ok := parseRefusal(stdout.Bytes()); ok {
				return nil, refusal
			}
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = fmt.Sprintf("resolver %s", exitErr.String())
			}

			return nil, &UnresolvableError{Reason: reason}
		}

		return nil, fmt.Errorf("run resolver: %w", err)
	}

	return stdout.Bytes(), nil
}

// resolverEnv is the environment of the resolver command: a fixed set of
// variables copied from the server, never the whole environment. The slice is
// non-nil on purpose: a nil Env would mean "inherit everything".
func resolverEnv() []string {
	env := make([]string, 0, 5)
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}

	return env
}

// boundedBuffer keeps at most limit bytes written to it. With stop set it
// fails the write past the limit and calls stop, which kills the process
// (stdout). With discard set the excess is silently dropped (stderr).
type boundedBuffer struct {
	limit    int64
	discard  bool
	stop     func()
	exceeded bool
	buf      bytes.Buffer
}

// Write implements io.Writer.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	room := b.limit - int64(b.buf.Len())
	if int64(len(p)) <= room {
		b.buf.Write(p)
		return len(p), nil
	}

	if room > 0 {
		b.buf.Write(p[:room])
	}

	if b.discard {
		return len(p), nil
	}

	b.exceeded = true
	if b.stop != nil {
		b.stop()
	}

	return 0, errOutputExceeded
}

// Bytes returns the kept bytes.
func (b *boundedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

// String returns the kept bytes as a string.
func (b *boundedBuffer) String() string {
	return b.buf.String()
}
