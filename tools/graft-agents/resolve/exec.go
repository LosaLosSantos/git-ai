package resolve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// maxCapturedStderr bounds the stderr kept for diagnostics.
const maxCapturedStderr = 4 << 10

// baseEnv names the environment variables every subprocess receives. Anything
// else in graft-resolve's environment, such as provider API keys, is withheld
// unless a strategy or check lists the variable in its env option.
var baseEnv = []string{
	"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "LANG", "LC_ALL", "LC_CTYPE", "SYSTEMROOT",
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateEnvNames checks the names of an env allowlist.
func validateEnvNames(names []string) error {
	for _, n := range names {
		if !envNameRE.MatchString(n) {
			return fmt.Errorf("invalid environment variable name %q", n)
		}
	}
	return nil
}

// minimalEnv returns the environment for a subprocess: the base variables plus
// the extra names, copied from the current process when set. It is never nil,
// because a nil Cmd.Env would inherit the whole environment.
func minimalEnv(extra []string) []string {
	env := []string{}
	seen := map[string]bool{}
	for _, name := range append(append([]string{}, baseEnv...), extra...) {
		if seen[name] {
			continue
		}
		seen[name] = true
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// execSpec describes one subprocess run.
type execSpec struct {
	dir     string
	argv    []string
	timeout time.Duration
	// env lists extra environment variables to pass through.
	env []string
	// stdoutLimit > 0 captures stdout, keeping at most stdoutLimit+1 bytes so
	// that the caller can detect an overflow. Zero discards stdout.
	stdoutLimit int64
}

// execute runs argv (no shell) in spec.dir with a timeout. It returns the
// captured stdout and the bounded stderr. The error mentions a timeout when the
// deadline ended the run.
func execute(ctx context.Context, spec execSpec) ([]byte, string, error) {
	if len(spec.argv) == 0 || spec.argv[0] == "" {
		return nil, "", errors.New("empty command")
	}
	ctx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, spec.argv[0], spec.argv[1:]...)
	cmd.Dir = spec.dir
	cmd.Env = minimalEnv(spec.env)
	var stdout *limitedBuffer
	if spec.stdoutLimit > 0 {
		stdout = &limitedBuffer{limit: int(spec.stdoutLimit) + 1}
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = io.Discard
	}
	stderr := &limitedBuffer{limit: maxCapturedStderr}
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	text := strings.TrimSpace(stderr.String())
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, text, fmt.Errorf("timed out after %s", spec.timeout)
		}
		return nil, text, err
	}
	if stdout == nil {
		return nil, text, nil
	}
	return stdout.bytes(), text, nil
}

// runCommand executes argv (no shell) in dir with a timeout and returns the
// captured, bounded stderr. Stdin is empty, stdout is discarded and the
// environment is the minimal one: programs that produce results write files
// named by placeholders.
func runCommand(ctx context.Context, dir string, argv []string, timeout time.Duration) (string, error) {
	_, stderr, err := execute(ctx, execSpec{dir: dir, argv: argv, timeout: timeout})
	return stderr, err
}

// limitedBuffer keeps at most limit bytes and discards the rest.
type limitedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func (b *limitedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...)
}
