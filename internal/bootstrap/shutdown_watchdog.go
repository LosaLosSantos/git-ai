package bootstrap

import (
	"bytes"
	"cmp"
	"regexp"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/log"
)

const (
	// minShutdownWatchdogTimeout is the floor for the watchdog timeout. It keeps
	// the watchdog from firing spuriously when a very small graceful restart
	// timeout is configured.
	minShutdownWatchdogTimeout = 30 * time.Second

	// maxReportedGoroutineStacks bounds how many goroutine stacks are emitted to
	// the log so a wedged shutdown cannot flood the log pipeline. Each stack is a
	// single log record, so this is set generously to make sure we capture the
	// blocker; the per-state histogram still gives the full picture.
	maxReportedGoroutineStacks = 50

	// maxReportedStackFrames trims each reported stack to its top frames. The top
	// frames identify the blocker; the deep runtime frames rarely matter, and a
	// short field is far less likely to be dropped by the log pipeline's
	// per-line/field size limits.
	maxReportedStackFrames = 16
)

// goroutineHeader parses the header line of a goroutine in a debug=2 dump, e.g.
// "goroutine 42 [chan receive, 14 minutes]:". It captures the id, the state, and
// the optional minute-granularity duration the runtime adds once a goroutine has
// been blocked for about a minute (so a goroutine blocked for less than that has
// no duration). The duration, when present, always follows the state directly,
// even when the runtime appends more annotations after it such as
// "[chan receive, 5 minutes, locked to thread]".
var goroutineHeader = regexp.MustCompile(`^goroutine (\d+) \[([^,\]]+)(?:, (\d+) minutes)?`)

// ShutdownWatchdogTimeout returns how long shutdown may take before the watchdog
// fires. It allows generous headroom over the grace period so the watchdog only
// fires on a genuine stall.
func ShutdownWatchdogTimeout(graceTimeout time.Duration) time.Duration {
	return max(2*graceTimeout, minShutdownWatchdogTimeout)
}

// StartShutdownWatchdog arms a watchdog that, if shutdown does not complete
// within timeout, logs the stacks of the goroutines that are holding it up. It
// must be armed only once shutdown has begun (after the graceful stop period has
// elapsed), otherwise it would fire during normal operation.
//
// The watchdog only observes: it logs diagnostics and does not change shutdown
// behavior. On a clean shutdown the process exits before the timeout, so it never
// fires; the returned timer is therefore usually ignored, but callers may Stop it
// if they want to disarm the watchdog explicitly.
func StartShutdownWatchdog(logger log.Logger, timeout time.Duration) *time.Timer {
	return time.AfterFunc(timeout, func() { reportShutdownStall(logger, timeout) })
}

// reportedGoroutine is a single goroutine selected as a likely blocker of
// shutdown.
type reportedGoroutine struct {
	// id is the goroutine number from the dump header.
	id int
	// state is the goroutine's wait state, e.g. "chan receive".
	state string
	// minutes is the wait duration the runtime reported, or 0 if it was blocked
	// for less than a minute (in which case the runtime omits the duration).
	minutes int
	// stack is the trimmed stack of the goroutine.
	stack string
}

// reportShutdownStall captures a goroutine dump and logs the essential parts: a
// per-state histogram plus the trimmed stacks of the goroutines most likely to
// be holding up shutdown.
func reportShutdownStall(logger log.Logger, timeout time.Duration) {
	logger.WithField("shutdown_timeout", timeout.String()).
		Error("[shutdown] shutdown exceeded timeout; capturing goroutine dump")

	var buf bytes.Buffer
	// The debug=2 dump is capped at 64 MB by the runtime (writeGoroutineStacks):
	// beyond that the trace is truncated. That bounds our memory, and truncation
	// only happens at an extreme goroutine count (~150k at ~420 B each); a normal
	// shutdown stall is far smaller, so we accept a truncated dump in that
	// pathological case.
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
		logger.WithError(err).Error("[shutdown] failed to capture goroutine dump")
		return
	}

	total, histogram, reported := extractStalledGoroutines(buf.Bytes(), maxReportedGoroutineStacks, maxReportedStackFrames)

	logger.WithFields(log.Fields{
		"total_goroutines": total,
		"states":           histogram,
	}).Error("[shutdown] goroutine summary")

	for _, g := range reported {
		logger.WithFields(log.Fields{
			"goroutine_id":    g.id,
			"state":           g.state,
			"blocked_minutes": g.minutes,
			"stack":           g.stack,
		}).Error("[shutdown] goroutine likely blocking shutdown")
	}
}

// extractStalledGoroutines parses a debug=2 goroutine dump and returns the total
// goroutine count, a per-state histogram, and a sample of the goroutines most
// likely to be holding up shutdown. The main goroutine (id 1) is always included
// first, since it shows where run() itself is stuck (e.g. inside
// gitalyServerFactory.Stop()). Actively running goroutines are otherwise
// excluded; the rest are ranked longest-blocked first (a goroutine blocked for
// less than a minute has no reported duration and ranks after those that do),
// capped at maxStacks and trimmed to maxFrames frames each.
func extractStalledGoroutines(dump []byte, maxStacks, maxFrames int) (int, map[string]int, []reportedGoroutine) {
	blocks := splitGoroutineBlocks(dump)
	histogram := make(map[string]int)

	var reported []reportedGoroutine
	for _, block := range blocks {
		id, state, minutes := 0, "unknown", 0
		if m := goroutineHeader.FindSubmatch(block); m != nil {
			id, _ = strconv.Atoi(string(m[1]))
			state = string(m[2])
			minutes, _ = strconv.Atoi(string(m[3])) // m[3] is empty when there is no duration
		}

		histogram[state]++

		// Always keep the main goroutine; otherwise skip goroutines that are
		// actively running rather than blocked.
		if id != 1 && (state == "running" || state == "runnable") {
			continue
		}

		reported = append(reported, reportedGoroutine{
			id:      id,
			state:   state,
			minutes: minutes,
			stack:   trimStack(block, maxFrames),
		})
	}

	// Main goroutine first, then longest-blocked first so the goroutines that
	// have been stuck the longest are reported even when the sample is capped.
	slices.SortStableFunc(reported, func(a, b reportedGoroutine) int {
		switch {
		case a.id == 1 && b.id != 1:
			return -1
		case a.id != 1 && b.id == 1:
			return 1
		default:
			return cmp.Compare(b.minutes, a.minutes)
		}
	})

	if len(reported) > maxStacks {
		reported = reported[:maxStacks]
	}
	return len(blocks), histogram, reported
}

// splitGoroutineBlocks splits a debug=2 goroutine dump into one block per
// goroutine. The returned blocks are subslices of dump, so it does not copy the
// (up to 64 MB) dump.
func splitGoroutineBlocks(dump []byte) [][]byte {
	var blocks [][]byte
	for block := range bytes.SplitSeq(dump, []byte("\n\n")) {
		if block = bytes.TrimSpace(block); len(block) > 0 {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// trimStack keeps a goroutine block's header and its top maxFrames frames. A
// frame is two lines in the dump (the function call and its file:line), so we
// keep 1+2*maxFrames lines and mark the rest as truncated. Only the blocks we
// actually report reach this function, so this is where the dump is first copied
// into a string.
func trimStack(block []byte, maxFrames int) string {
	lines := strings.Split(strings.TrimRight(string(block), "\n"), "\n")

	keep := 1 + 2*maxFrames
	if len(lines) <= keep {
		return string(block)
	}

	trimmed := make([]string, 0, keep+1)
	trimmed = append(trimmed, lines[:keep]...)
	trimmed = append(trimmed, "\t...[stack truncated]")
	return strings.Join(trimmed, "\n")
}
