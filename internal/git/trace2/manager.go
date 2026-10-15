package trace2

import (
	"context"
	"fmt"
	"os"
)

// Manager is responsible for enabling Trace2 for a Git command. It manages the list of hooks who
// are interested in trace2 data. Before the command starts, the manager opens a tempfile. It
// injects the path to this file and some other conventional environment variables to the ENV list
// of the command by calling Inject. After the command exits, the caller is expected to call Finish.
// Finally, the transformed trace2 tree is passed into handlers of registered hooks.
type Manager struct {
	sid   string
	hooks []Hook
	fd    *os.File
	err   error
}

// NewManager returns a Manager object that manages the registered hooks
func NewManager(sid string, hooks []Hook) (*Manager, error) {
	if len(hooks) == 0 {
		return nil, fmt.Errorf("input hooks are empty")
	}
	return &Manager{sid: sid, hooks: hooks}, nil
}

// HookNames return names of hooks
func (m *Manager) HookNames() []string {
	var names []string
	for _, hook := range m.hooks {
		names = append(names, hook.Name())
	}
	return names
}

// Error returns the error occurs after the manager finishes
func (m *Manager) Error() error {
	return m.err
}

// Inject injects the path to the tempfile used to store trace2 events and conventional environment
// variables to the input ENV list.
func (m *Manager) Inject(env []string) []string {
	fd, err := os.CreateTemp("", "gitaly-trace2")
	if err != nil {
		m.err = fmt.Errorf("trace2 create tempfile: %w", err)
		return env
	}
	m.fd = fd

	env = append(
		env,
		// GIT_TRACE2_EVENT is the key ENV variable. It enables git to dump event format
		// target as JSON-based format. When the path to the file is supplied, it *appends*
		// the events to the appointed file. Child processes inherits the same ENV set.
		// Thus, their events are dumped to the same file. This file is cleaned up when
		// calling Finish().
		fmt.Sprintf("GIT_TRACE2_EVENT=%s", fd.Name()),
		// GIT_TRACE2_PARENT_SID is the unique identifier of a process. As PID number is
		// re-used, Git uses SID number to identify the owner of an event
		fmt.Sprintf("GIT_TRACE2_PARENT_SID=%s", m.sid),
		// GIT_TRACE2_BRIEF strips redundant information, such as time, file, line, etc.
		// This variable makes the output data compact enough to use on production. One
		// notable stripped field is time. The time information is available in some key
		// events. Subsequent events must infer their time from relative float time diff.
		"GIT_TRACE2_BRIEF=true",
		// Apart from the above variables, there are some non-documented interesting
		// variables, such as GIT_TRACE2_CONFIG_PARAMS or GIT_TRACE2_ENV_VARS. We can
		// consider adding them in the future. The full list can be found at:
		// https://github.com/git/git/blob/master/trace2.h
	)
	return env
}

// Finish reads the events, parses them to a tree, triggers hook handlers, and clean up the fd.
func (m *Manager) Finish(ctx context.Context) {
	if m.Error() != nil {
		return
	}

	defer func() {
		if err := m.fd.Close(); err != nil {
			if m.err == nil {
				m.err = fmt.Errorf("trace2: close tempfile: %w", err)
			}
			// Even if the manager fails to close the tempfile, it should fallthrough to
			// remove it from the file system
		}
		if err := os.Remove(m.fd.Name()); err != nil {
			if m.err == nil {
				m.err = fmt.Errorf("trace2: remove tempfile: %w", err)
			}
		}
	}()

	trace, err := Parse(ctx, m.fd)
	if err != nil {
		m.err = fmt.Errorf("trace2: parsing events: %w", err)
		return
	}
	if trace == nil {
		m.err = fmt.Errorf("trace2: no events to handle")
		return
	}
	for _, hook := range m.hooks {
		if err := hook.Handle(ctx, trace); err != nil {
			m.err = fmt.Errorf("trace2: executing %q handler: %w", hook.Name(), err)
			return
		}
	}
}
