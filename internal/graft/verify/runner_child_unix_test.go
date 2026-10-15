//go:build unix

package verify

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A passing command that leaves a background child holding its output pipes
// is a passed test. The child is killed, so that it does not outlive the run.
func TestRunTestsBackgroundChildKeepingOutputPasses(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	tc := TestConfig{
		Command: []string{"sh", "-c", "sleep 30 & echo $! > " + pidFile + "; exit 0"},
		Timeout: Duration(time.Minute),
	}

	res, err := runTests(context.Background(), t.TempDir(), tc, "abc")
	require.NoError(t, err)
	require.True(t, res.Passed, "the exit status 0 of the command is the verdict")
	require.False(t, res.TimedOut)
	require.Equal(t, 0, res.ExitCode)

	pid := readPIDFile(t, pidFile)
	require.Eventually(t, func() bool { return processGone(pid) }, 5*time.Second, 10*time.Millisecond,
		"background child %d of a passed test command is still running", pid)
}

// A background child of a failing command is killed too.
func TestRunTestsBackgroundChildOfFailingCommandIsKilled(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	tc := TestConfig{
		Command: []string{"sh", "-c", "sleep 30 & echo $! > " + pidFile + "; exit 3"},
		Timeout: Duration(time.Minute),
	}

	res, err := runTests(context.Background(), t.TempDir(), tc, "abc")
	require.NoError(t, err)
	require.False(t, res.Passed)
	require.Equal(t, 3, res.ExitCode)

	pid := readPIDFile(t, pidFile)
	require.Eventually(t, func() bool { return processGone(pid) }, 5*time.Second, 10*time.Millisecond,
		"background child %d of a failed test command is still running", pid)
}

// A run of RunOnce with such a command moves the green ref, and the cursor
// moves with it: the run is not an error that is retried forever.
func TestRunOnceBackgroundChildKeepingOutputMovesGreen(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	fx := leftoverFixture(t, "sleep 30 & echo $! > "+pidFile+"; exit 0")
	c1 := fx.commit("a.txt", "one")
	v := fx.verifier()

	out, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.True(t, out.Passed)
	require.Equal(t, c1, out.Tip)
	require.Equal(t, c1, fx.green())
	require.Equal(t, c1, out.Green)

	pid := readPIDFile(t, pidFile)
	require.Eventually(t, func() bool { return processGone(pid) }, 5*time.Second, 10*time.Millisecond,
		"background child %d left by the test run is still running", pid)
}

// A FIFO left in the tree by a test run is a leftover too, and it must not
// make a later commit fail or block the cleanup.
func TestRunOnceFIFOLeftoverIsRemovedForLaterCommit(t *testing.T) {
	fx := leftoverFixture(t, `if [ -p pipe ]; then exit 1; fi; mkfifo pipe; exit 0`)
	fx.commit("a.txt", "one")
	v := fx.verifier()

	out1, _, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, out1.Passed)

	c2 := fx.commit("b.txt", "two")
	out2, ran, err := v.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, c2, out2.Tip)
	require.True(t, out2.Passed, "the FIFO left by the previous run must not be in the tree")
	require.Equal(t, c2, fx.green())
}

// readPIDFile returns the process ID that the test command wrote to path.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	require.Positive(t, pid)
	return pid
}
