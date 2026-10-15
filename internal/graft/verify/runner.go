package verify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

// maxOutputBytes bounds the output kept from one test run. The tail is kept,
// because the end of a test log holds the failure.
const maxOutputBytes = 64 << 10

// waitDelay bounds how long a test run may keep its output pipes open after
// it was killed, for example by a child process that inherited them.
const waitDelay = 5 * time.Second

// TestResult is the verdict of one run of the test command.
type TestResult struct {
	Commit   string        `json:"commit"`
	Passed   bool          `json:"passed"`
	TimedOut bool          `json:"timed_out,omitempty"`
	ExitCode int           `json:"exit_code"`
	Output   string        `json:"output,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

// runTests runs the test command of tc in dir for commit. A command that
// exits non-zero, or that exceeds the timeout, is a failed test and not an
// error. An error means that the command could not be run, or that ctx was
// canceled: nothing is known about the commit then.
func runTests(ctx context.Context, dir string, tc TestConfig, commit string) (TestResult, error) {
	if len(tc.Command) == 0 {
		return TestResult{}, errors.New("test command is empty")
	}

	runCtx, cancel := context.WithTimeout(ctx, tc.Timeout.Std())
	defer cancel()

	cmd := exec.CommandContext(runCtx, tc.Command[0], tc.Command[1:]...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), tc.Env...), "GRAFT_VERIFY_COMMIT="+commit)
	cmd.WaitDelay = waitDelay
	// A timeout kills the whole process group, so that the children of the
	// command (for example the test binary of go test) stop too.
	killProcessGroup(cmd)

	out := &tailBuffer{limit: maxOutputBytes}
	cmd.Stdout = out
	cmd.Stderr = out

	start := time.Now()
	err := cmd.Run()
	// The command has exited, or was killed. A child that it left running, for
	// example `./server &`, must not outlive the run: the whole process group
	// goes, whatever the outcome.
	killGroup(cmd)

	result := TestResult{
		Commit:   commit,
		Duration: time.Since(start),
	}
	result.Output = out.String()

	if ctx.Err() != nil {
		return TestResult{}, ctx.Err()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		result.ExitCode = -1
		return result, nil
	}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		result.Passed = true
		return result, nil
	case errors.Is(err, exec.ErrWaitDelay):
		// The command exited with status 0, but a child kept its output pipes
		// open for longer than WaitDelay. The exit status is the verdict.
		result.Passed = true
		return result, nil
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	default:
		return TestResult{}, err
	}
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	limit int
	buf   bytes.Buffer
}

// Write implements io.Writer. It never fails, so that a chatty command is not
// stopped by its output.
func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > t.limit {
		p = p[len(p)-t.limit:]
	}
	t.buf.Write(p)
	if extra := t.buf.Len() - t.limit; extra > 0 {
		t.buf.Next(extra)
	}
	return n, nil
}

// String returns the kept tail.
func (t *tailBuffer) String() string {
	return t.buf.String()
}
