package burdenmonitor

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var commandsEvictedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gitaly_burdenmonitor_commands_evicted_total",
		Help: "Total number of commands evicted from BurdenMonitor tracking",
	},
	[]string{"grpc_service", "grpc_method"},
)

// maxTrackedCommands caps how many commands an RPC entry tracks at once. A
// survey of RPCs yielded at most 10 distinct Git commands. Some Git commands
// are accompanied by a git-rev-parse(1) invocation to detect the hash format.
//
// Some RPCs spawn a variable number of Git commands depending on the shape of
// the user input. Those are typically very short-lived commands, so excluding
// the cheaper ones should not affect burden monitoring.
const maxTrackedCommands = 20

// CommandStats holds resource usage statistics for a spawned command.
type CommandStats struct {
	Name      string    `json:"name"`
	Pid       int       `json:"pid"`
	StartTime time.Time `json:"start_time"`

	UserTime   time.Duration `json:"user_time"`
	SystemTime time.Duration `json:"system_time"`
	WallTime   time.Duration `json:"wall_time"`
	AnonRSS    int64         `json:"anon_rss"`
}

// TODO: use a custom json marshaller that will populate total CPU time and
// total memory for the list of commands.

// RPCEntry represents an active RPC and its associated commands.
type RPCEntry struct {
	ID            string                  `json:"id"`
	ServiceName   string                  `json:"service_name"`
	MethodName    string                  `json:"method_name"`
	StartTime     time.Time               `json:"start_time"`
	Context       context.Context         `json:"-"`
	Cancel        context.CancelCauseFunc `json:"-"`
	CorrelationID string                  `json:"correlation_id"`
	Repository    string                  `json:"repository"`

	mu       sync.RWMutex
	Commands map[int]*CommandStats `json:"commands"`

	completedCPUTime time.Duration
	completedCount   int
}

// TotalCPUTime returns the sum of user and system CPU time across running commands.
func (e *RPCEntry) TotalCPUTime() time.Duration {
	e.mu.RLock()
	defer e.mu.RUnlock()

	total := e.completedCPUTime
	for _, cmd := range e.Commands {
		total += cmd.UserTime + cmd.SystemTime
	}
	return total
}

// TotalMemory returns the sum of anonymous RSS memory across all commands.
func (e *RPCEntry) TotalMemory() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var total int64
	for _, cmd := range e.Commands {
		total += cmd.AnonRSS
	}
	return total
}

// ActiveCommandCount returns the number of commands that have not yet completed.
func (e *RPCEntry) ActiveCommandCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return len(e.Commands)
}

// commandsSnapshot returns a copy of all command stats. The copies can be read
// without holding the entry's lock.
func (e *RPCEntry) commandsSnapshot() []CommandStats {
	e.mu.RLock()
	defer e.mu.RUnlock()

	cmds := make([]CommandStats, 0, len(e.Commands))
	for _, cmd := range e.Commands {
		cmds = append(cmds, *cmd)
	}
	return cmds
}

// RegisterCommand adds a new command to the RPC entry's tracking.
func (e *RPCEntry) RegisterCommand(pid int, name string, startTime time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, ok := e.Commands[pid]; !ok && len(e.Commands) >= maxTrackedCommands {
		e.evictCommand()
	}

	e.Commands[pid] = &CommandStats{
		Name:      name,
		Pid:       pid,
		StartTime: startTime,
	}
}

// evictCommand removes the tracked command with the lowest CPU consumption.
// Its resource consumption will be added to the aggregate during the
// MarkCommandCompleted call.
func (e *RPCEntry) evictCommand() {
	var victim *CommandStats
	for _, cmd := range e.Commands {
		if victim == nil || cmd.UserTime+cmd.SystemTime < victim.UserTime+victim.SystemTime {
			victim = cmd
		}
	}
	if victim == nil {
		return
	}

	delete(e.Commands, victim.Pid)

	commandsEvictedTotal.WithLabelValues(e.ServiceName, e.MethodName).Inc()
}

// MarkCommandCompleted removes a completed command from the entry's tracking,
// folding its final CPU times into the entry's aggregates.
func (e *RPCEntry) MarkCommandCompleted(pid int, userTime, systemTime time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.Commands, pid)

	e.completedCPUTime += userTime + systemTime
	e.completedCount++
}
