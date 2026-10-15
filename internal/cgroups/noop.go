package cgroups

import (
	"io"
	"os/exec"
	"slices"
	"sync"
)

// NoopManager is a cgroups manager that does nothing
type NoopManager struct{}

//nolint:revive // This is unintentionally missing documentation.
func (cg *NoopManager) Ready() bool {
	return false
}

//nolint:revive // This is unintentionally missing documentation.
func (cg *NoopManager) Stats() (Stats, error) {
	return Stats{
		isEmpty: true,
		version: 0,
	}, nil
}

//nolint:revive // This is unintentionally missing documentation.
func (cg *NoopManager) Setup() error {
	return nil
}

//nolint:revive // This is unintentionally missing documentation.
func (cg *NoopManager) AddCommand(*exec.Cmd, ...AddCommandOption) (string, error) {
	return "", nil
}

// SupportsCloneIntoCgroup returns false.
func (cg *NoopManager) SupportsCloneIntoCgroup() bool {
	return false
}

// CloneIntoCgroup does nothing.
func (cg *NoopManager) CloneIntoCgroup(*exec.Cmd, ...AddCommandOption) (string, io.Closer, error) {
	return "", io.NopCloser(nil), nil
}

// RecordedCommand is a command that was cloned into a cgroup together with the key of that cgroup.
type RecordedCommand struct {
	Key string
	Cmd *exec.Cmd
}

// MockManager does nothing except return the cgroup key set through options and record every
// command cloned into a cgroup in the order the calls were made. It is safe for concurrent use.
type MockManager struct {
	NoopManager

	mu       sync.Mutex
	commands []RecordedCommand
}

// CloneIntoCgroup records the command and returns the cgroup key that is set through options.
func (mm *MockManager) CloneIntoCgroup(cmd *exec.Cmd, opts ...AddCommandOption) (string, io.Closer, error) {
	var cfg addCommandCfg
	for _, opt := range opts {
		opt(&cfg)
	}

	mm.mu.Lock()
	defer mm.mu.Unlock()
	mm.commands = append(mm.commands, RecordedCommand{Key: cfg.cgroupKey, Cmd: cmd})

	return cfg.cgroupKey, io.NopCloser(nil), nil
}

// SupportsCloneIntoCgroup returns true
func (mm *MockManager) SupportsCloneIntoCgroup() bool {
	return true
}

// Commands returns a copy of all recorded commands in the order they were cloned into a cgroup.
func (mm *MockManager) Commands() []RecordedCommand {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	return slices.Clone(mm.commands)
}
