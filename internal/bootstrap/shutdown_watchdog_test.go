package bootstrap

import (
	"bytes"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestShutdownWatchdogTimeout(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc     string
		grace    time.Duration
		expected time.Duration
	}{
		{desc: "small grace uses floor", grace: time.Second, expected: 30 * time.Second},
		{desc: "zero grace uses floor", grace: 0, expected: 30 * time.Second},
		{desc: "large grace doubled", grace: time.Minute, expected: 2 * time.Minute},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.expected, ShutdownWatchdogTimeout(tc.grace))
		})
	}
}

// blockedInTestOnChannelReceive is a distinctively named helper so a real
// goroutine dump taken while it is parked can be asserted against.
func blockedInTestOnChannelReceive(release <-chan struct{}) {
	<-release
}

// TestExtractStalledGoroutines_realDump validates the extraction against a real
// pprof goroutine dump (not synthetic text): it parks several goroutines and
// asserts the extraction surfaces them with the right state and stack. This is
// what makes the watchdog's output trustworthy on an actual stall.
func TestExtractStalledGoroutines_realDump(t *testing.T) {
	t.Parallel()

	const parked = 3
	release := make(chan struct{})
	defer close(release)

	for range parked {
		go blockedInTestOnChannelReceive(release)
	}

	// Poll a live dump until the extraction surfaces all parked goroutines. This
	// both waits for them to block and exercises extractStalledGoroutines against
	// real runtime output, with no dependency on pre-baked text.
	require.Eventually(t, func() bool {
		var buf bytes.Buffer
		require.NoError(t, pprof.Lookup("goroutine").WriteTo(&buf, 2))

		_, histogram, reported := extractStalledGoroutines(buf.Bytes(), 100, maxReportedStackFrames)
		if histogram["chan receive"] < parked {
			return false
		}

		surfaced := 0
		for _, g := range reported {
			// Actively running goroutines must be excluded (unless it is the main
			// goroutine, id 1, which is always kept).
			if g.id != 1 {
				require.NotContains(t, []string{"running", "runnable"}, g.state)
			}
			if strings.Contains(g.stack, "blockedInTestOnChannelReceive") {
				surfaced++
				require.Equal(t, "chan receive", g.state)
			}
		}
		return surfaced >= parked
	}, 5*time.Second, 10*time.Millisecond)
}

func TestSplitGoroutineBlocks(t *testing.T) {
	t.Parallel()

	dump := "goroutine 1 [running]:\nmain.a()\n\tfoo.go:1 +0x1\n\n" +
		"goroutine 2 [chan receive, 14 minutes]:\nmain.b()\n\tbar.go:2 +0x2\n"

	blocks := splitGoroutineBlocks([]byte(dump))
	require.Len(t, blocks, 2)
	require.Contains(t, string(blocks[0]), "goroutine 1 [running]:")
	require.Contains(t, string(blocks[1]), "goroutine 2 [chan receive, 14 minutes]:")
}

func TestExtractStalledGoroutines_histogramAndTotal(t *testing.T) {
	t.Parallel()

	dump := strings.Join([]string{
		"goroutine 10 [running]:\nmain.a()\n\ta.go:1 +0x1",
		"goroutine 11 [chan receive, 14 minutes]:\nmain.b()\n\tb.go:2 +0x2",
		"goroutine 12 [chan receive]:\nmain.c()\n\tc.go:3 +0x3",
	}, "\n\n")

	total, histogram, _ := extractStalledGoroutines([]byte(dump), 50, maxReportedStackFrames)

	require.Equal(t, 3, total)
	require.Equal(t, map[string]int{"running": 1, "chan receive": 2}, histogram)
}

func TestExtractStalledGoroutines_rankingAndCap(t *testing.T) {
	t.Parallel()

	dump := strings.Join([]string{
		"goroutine 10 [running]:\nmain.busy()\n\tbusy.go:1 +0x1",
		"goroutine 11 [chan receive]:\nmain.short()\n\tshort.go:2 +0x2",
		// Thread-locked, so the duration is not at the end of the header. It must
		// still be parsed, otherwise this genuinely long-blocked goroutine would
		// be misranked and dropped by the cap.
		"goroutine 12 [semacquire, 9 minutes, locked to thread]:\nmain.longest()\n\tlong.go:3 +0x3",
		"goroutine 13 [select, 2 minutes]:\nmain.middle()\n\tmid.go:4 +0x4",
	}, "\n\n")

	_, _, reported := extractStalledGoroutines([]byte(dump), 2, maxReportedStackFrames)

	// The running goroutine is excluded; the rest are ranked longest-blocked
	// first and capped at 2, so the 9-minute and 2-minute goroutines win over the
	// duration-less one.
	require.Len(t, reported, 2)
	require.Equal(t, 9, reported[0].minutes)
	require.Contains(t, reported[0].stack, "main.longest()")
	require.Equal(t, 2, reported[1].minutes)
	require.Contains(t, reported[1].stack, "main.middle()")
}

func TestExtractStalledGoroutines_mainAlwaysFirst(t *testing.T) {
	t.Parallel()

	dump := strings.Join([]string{
		// A long-idle background goroutine that would otherwise outrank everything.
		"goroutine 99 [select, 240 minutes]:\nmain.idle()\n\tidle.go:1 +0x1",
		// The main goroutine, blocked for a shorter time (where run() is stuck).
		"goroutine 1 [semacquire, 1 minutes]:\nmain.main()\n\tmain.go:2 +0x2",
	}, "\n\n")

	_, _, reported := extractStalledGoroutines([]byte(dump), 50, maxReportedStackFrames)

	require.NotEmpty(t, reported)
	require.Equal(t, 1, reported[0].id, "main goroutine must be reported first")
	require.Contains(t, reported[0].stack, "main.main()")
}

func TestGoroutineHeader(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc    string
		header  string
		id      int
		state   string
		minutes int
	}{
		{desc: "no duration", header: "goroutine 9 [chan receive]:", id: 9, state: "chan receive"},
		{desc: "with duration", header: "goroutine 42 [chan receive, 14 minutes]:", id: 42, state: "chan receive", minutes: 14},
		{desc: "running", header: "goroutine 3 [running]:", id: 3, state: "running"},
		// The runtime appends ", locked to thread" after the duration; it must
		// still be parsed, and must not be parsed when there is no duration.
		{desc: "duration then locked", header: "goroutine 5 [chan receive, 5 minutes, locked to thread]:", id: 5, state: "chan receive", minutes: 5},
		{desc: "locked without duration", header: "goroutine 7 [syscall, locked to thread]:", id: 7, state: "syscall"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			m := goroutineHeader.FindStringSubmatch(tc.header)
			require.NotNil(t, m, tc.header)

			id, _ := strconv.Atoi(m[1])
			minutes, _ := strconv.Atoi(m[3])
			require.Equal(t, tc.id, id)
			require.Equal(t, tc.state, m[2])
			require.Equal(t, tc.minutes, minutes)
		})
	}
}

func TestTrimStack(t *testing.T) {
	t.Parallel()

	t.Run("keeps short stacks whole", func(t *testing.T) {
		block := "goroutine 1 [running]:\nmain.a()\n\tfoo.go:1 +0x1"
		require.Equal(t, block, trimStack([]byte(block), 16))
	})

	t.Run("truncates long stacks to top frames", func(t *testing.T) {
		block := "goroutine 1 [running]:\n" +
			"main.a()\n\tfoo.go:1 +0x1\n" +
			"main.b()\n\tbar.go:2 +0x2\n" +
			"main.c()\n\tbaz.go:3 +0x3"

		trimmed := trimStack([]byte(block), 1)
		require.Equal(t,
			"goroutine 1 [running]:\nmain.a()\n\tfoo.go:1 +0x1\n\t...[stack truncated]",
			trimmed,
		)
	})
}
