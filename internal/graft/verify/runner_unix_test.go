//go:build unix

package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A timeout kills the children of the test command too, not only the command.
func TestRunTestsTimeoutKillsChildProcesses(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	tc := TestConfig{
		Command: []string{"sh", "-c", "sleep 30 & echo $! > " + pidFile + "; wait"},
		Timeout: Duration(time.Second),
	}

	res, err := runTests(context.Background(), t.TempDir(), tc, "abc")
	require.NoError(t, err)
	require.True(t, res.TimedOut)

	data, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return processGone(pid) }, 5*time.Second, 10*time.Millisecond,
		"child process %d of a timed out test command is still running", pid)
}

// processGone reports whether pid no longer runs. A killed process stays a
// zombie until its parent reaps it, which counts as gone.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	return bytes.Contains(stat, []byte(") Z"))
}
