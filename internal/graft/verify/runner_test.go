package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// shell returns a test command that runs script with sh.
func shell(script string) TestConfig {
	return TestConfig{
		Command: []string{"sh", "-c", script},
		Timeout: Duration(30 * time.Second),
	}
}

func TestRunTestsPassingCommand(t *testing.T) {
	res, err := runTests(context.Background(), t.TempDir(), shell("exit 0"), "abc123")
	require.NoError(t, err)
	require.True(t, res.Passed)
	require.False(t, res.TimedOut)
	require.Equal(t, 0, res.ExitCode)
	require.Equal(t, "abc123", res.Commit)
}

func TestRunTestsFailingCommand(t *testing.T) {
	res, err := runTests(context.Background(), t.TempDir(), shell("echo boom; exit 3"), "abc")
	require.NoError(t, err)
	require.False(t, res.Passed)
	require.False(t, res.TimedOut)
	require.Equal(t, 3, res.ExitCode)
	require.Contains(t, res.Output, "boom")
}

func TestRunTestsCapturesStdoutAndStderr(t *testing.T) {
	res, err := runTests(context.Background(), t.TempDir(), shell("echo out-line; echo err-line >&2; exit 1"), "abc")
	require.NoError(t, err)
	require.Contains(t, res.Output, "out-line")
	require.Contains(t, res.Output, "err-line")
}

func TestRunTestsRunsInDirectoryWithCommitEnvironment(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "marker"), []byte("here"), 0o600))

	tc := shell(`test -f marker && test "$GRAFT_VERIFY_COMMIT" = deadbeef && test "$EXTRA" = yes`)
	tc.Env = []string{"EXTRA=yes"}

	res, err := runTests(context.Background(), dir, tc, "deadbeef")
	require.NoError(t, err)
	require.True(t, res.Passed, res.Output)
}

func TestRunTestsCommitVariableWinsOverConfiguredEnv(t *testing.T) {
	tc := shell(`test "$GRAFT_VERIFY_COMMIT" = real`)
	tc.Env = []string{"GRAFT_VERIFY_COMMIT=spoofed"}

	res, err := runTests(context.Background(), t.TempDir(), tc, "real")
	require.NoError(t, err)
	require.True(t, res.Passed, res.Output)
}

func TestRunTestsTimeoutIsFailure(t *testing.T) {
	tc := TestConfig{
		// exec replaces the shell, so the timeout kills the sleeping process
		// and the output pipes close at once.
		Command: []string{"sh", "-c", "exec sleep 30"},
		Timeout: Duration(200 * time.Millisecond),
	}

	start := time.Now()
	res, err := runTests(context.Background(), t.TempDir(), tc, "abc")
	require.NoError(t, err)
	require.False(t, res.Passed)
	require.True(t, res.TimedOut)
	require.Less(t, time.Since(start), 10*time.Second)
}

func TestRunTestsCommandCannotStartIsError(t *testing.T) {
	tc := TestConfig{
		Command: []string{filepath.Join(t.TempDir(), "no-such-program")},
		Timeout: Duration(time.Second),
	}

	_, err := runTests(context.Background(), t.TempDir(), tc, "abc")
	require.Error(t, err)
}

func TestRunTestsEmptyCommandIsError(t *testing.T) {
	_, err := runTests(context.Background(), t.TempDir(), TestConfig{Timeout: Duration(time.Second)}, "abc")
	require.ErrorContains(t, err, "test command is empty")
}

func TestRunTestsCanceledContextIsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runTests(ctx, t.TempDir(), shell("exit 0"), "abc")
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunTestsCancelDuringRunIsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	tc := TestConfig{Command: []string{"sh", "-c", "exec sleep 30"}, Timeout: Duration(time.Minute)}
	_, err := runTests(ctx, t.TempDir(), tc, "abc")
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunTestsOutputIsBounded(t *testing.T) {
	// Print far more than the limit; the tail, which holds the failure, must
	// be kept and the rest dropped.
	script := `i=0; while [ $i -lt 20000 ]; do echo "filler line number $i padding padding"; i=$((i+1)); done; echo THE-END; exit 1`
	res, err := runTests(context.Background(), t.TempDir(), shell(script), "abc")
	require.NoError(t, err)
	require.LessOrEqual(t, len(res.Output), maxOutputBytes)
	require.True(t, strings.HasSuffix(res.Output, "THE-END\n"), "tail kept")
	require.Contains(t, res.Output, "filler line number 19999")
	require.NotContains(t, res.Output, "filler line number 0 ")
}

func TestTailBufferKeepsLastBytes(t *testing.T) {
	tb := &tailBuffer{limit: 10}

	n, err := tb.Write([]byte("0123456789"))
	require.NoError(t, err)
	require.Equal(t, 10, n)
	require.Equal(t, "0123456789", tb.String())

	n, err = tb.Write([]byte("abcdefghijklmnop"))
	require.NoError(t, err)
	require.Equal(t, 16, n)
	require.Equal(t, "ghijklmnop", tb.String())

	_, _ = tb.Write([]byte("xy"))
	require.Equal(t, "ijklmnopxy", tb.String())
}
