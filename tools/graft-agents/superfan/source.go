package superfan

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// EventSource yields the events of the changeset log in order. Next returns
// io.EOF after the last event. Close releases the source.
type EventSource interface {
	Next(ctx context.Context) (Event, error)
	Close() error
}

// ReaderSource reads JSON lines (graft watch --json) from a reader, such as a
// file or standard input. A context cancellation is noticed between lines: a
// blocked read is not interrupted, so use CommandSource for a live watch.
type ReaderSource struct {
	sc     *bufio.Scanner
	closer io.Closer
	line   int
}

// NewReaderSource returns a source over r. closer, if not nil, is closed by Close.
func NewReaderSource(r io.Reader, closer io.Closer) *ReaderSource {
	return &ReaderSource{sc: newScanner(r), closer: closer}
}

// Next returns the next event or io.EOF.
func (s *ReaderSource) Next(ctx context.Context) (Event, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}
		if !s.sc.Scan() {
			if err := s.sc.Err(); err != nil {
				return Event{}, fmt.Errorf("read events: %w", err)
			}
			return Event{}, io.EOF
		}
		s.line++
		text, blank := trimLine(s.sc.Bytes())
		if blank {
			continue
		}
		ev, err := ParseEvent(text)
		if err != nil {
			return Event{}, fmt.Errorf("line %d: %w", s.line, err)
		}
		return ev, nil
	}
}

// Close closes the underlying reader when it has a closer.
func (s *ReaderSource) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

// maxCommandStderr bounds the stderr kept from a source command.
const maxCommandStderr = 4096

// CommandSource runs a command (for example `graft watch --json --after 7`)
// and reads its standard output as events. The command is started with
// exec, not a shell. Cancelling the context kills it.
type CommandSource struct {
	cmd    *exec.Cmd
	src    *ReaderSource
	stderr *cappedBuffer
	waited bool
}

// StartCommand starts argv and returns the source of its output.
func StartCommand(ctx context.Context, argv []string) (*CommandSource, error) {
	if len(argv) == 0 {
		return nil, errors.New("superfan: empty event command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("superfan: event command: %w", err)
	}
	stderr := &cappedBuffer{max: maxCommandStderr}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("superfan: start event command: %w", err)
	}
	return &CommandSource{cmd: cmd, src: NewReaderSource(stdout, nil), stderr: stderr}, nil
}

// Next returns the next event. When the command ends it returns io.EOF, or the
// failure of the command together with its stderr.
func (c *CommandSource) Next(ctx context.Context) (Event, error) {
	ev, err := c.src.Next(ctx)
	if errors.Is(err, io.EOF) {
		return Event{}, c.finish()
	}
	return ev, err
}

// finish waits for the command once and maps its exit to an error.
func (c *CommandSource) finish() error {
	if c.waited {
		return io.EOF
	}
	c.waited = true
	if err := c.cmd.Wait(); err != nil {
		if msg := bytes.TrimSpace(c.stderr.buf.Bytes()); len(msg) > 0 {
			return fmt.Errorf("superfan: event command failed: %w: %s", err, msg)
		}
		return fmt.Errorf("superfan: event command failed: %w", err)
	}
	return io.EOF
}

// Close kills the command if it still runs. It may be called while Next runs
// on another goroutine: the process is killed, and Next reaps it.
func (c *CommandSource) Close() error {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return nil
}

// cappedBuffer keeps the first max bytes written and discards the rest.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}
