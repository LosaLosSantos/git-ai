//go:build !unix

package verify

import "os/exec"

// killProcessGroup is a no-op where process groups are not available. The
// command itself is killed on timeout by exec.CommandContext.
func killProcessGroup(*exec.Cmd) {}

// killGroup kills the command, where process groups are not available. Its
// children are not reached.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
