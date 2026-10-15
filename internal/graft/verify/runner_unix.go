//go:build unix

package verify

import (
	"os/exec"
	"syscall"
)

// killProcessGroup makes cmd the leader of its own process group and kills the
// whole group when the context of cmd is done, so that no child of the test
// command keeps running after a timeout.
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

// killGroup kills the process group of cmd, which holds the children that the
// command left running after it exited. A group without members is not an
// error.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
