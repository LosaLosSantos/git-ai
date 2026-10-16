package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// oidA is a full object ID that errorKind removes from messages.
const oidA = "0123456789abcdef0123456789abcdef01234567"

func TestAggregatePercentilesCoverLandedChangesOnly(t *testing.T) {
	t.Parallel()

	// 100 landed changes of 1ms to 100ms and 50 failed changes of 9s. The
	// nearest-rank percentiles of the landed changes are their values 50, 95
	// and 99. The failed changes must not move them.
	var samples []Sample
	for i := 1; i <= 100; i++ {
		samples = append(samples, Sample{Landed: true, Latency: time.Duration(i) * time.Millisecond})
	}
	for i := 0; i < 50; i++ {
		samples = append(samples, Sample{Latency: 9 * time.Second, Err: errors.New("hook rejected")})
	}

	res := aggregate(baseConfig(), samples, time.Minute)

	require.Equal(t, 150, res.Changes)
	require.Equal(t, 100, res.Landed)
	require.Equal(t, 50, res.Failed)
	require.InDelta(t, 50, res.P50MS, 1e-9)
	require.InDelta(t, 95, res.P95MS, 1e-9)
	require.InDelta(t, 99, res.P99MS, 1e-9)
	require.InDelta(t, 9000, res.FailedP95MS, 1e-9)
}

func TestAggregateHistogramsAndFailureClasses(t *testing.T) {
	t.Parallel()

	samples := []Sample{
		{Landed: true, Outcome: LandOutcome{Attempts: 1, BatchSize: 1, BatchPosition: 1}},
		{Landed: true, Outcome: LandOutcome{Attempts: 1, BatchSize: 3, BatchPosition: 2}},
		{Landed: true, Outcome: LandOutcome{Attempts: 2, BatchSize: 3, BatchPosition: 1}},
		{Landed: true, Outcome: LandOutcome{AlreadyLanded: true}},
		{Err: errors.New("land: commit " + oidA + " not found"), Conflict: true, Outcome: LandOutcome{Attempts: 4}},
		{Err: errors.New("land: commit " + oidA + " not found"), Outcome: LandOutcome{Attempts: 1}},
		{Err: errors.New("land: hook rejected")},
	}

	res := aggregate(baseConfig(), samples, time.Second)

	require.Equal(t, 4, res.Landed)
	require.Equal(t, 1, res.AlreadyLanded)
	require.Equal(t, 3, res.Failed)
	require.Equal(t, 1, res.Conflicts)

	require.Equal(t, map[int]int{1: 2, 2: 1}, res.AttemptsHistogram,
		"only landed changes count, and a landed change without attempts is left out")
	require.Equal(t, map[int]int{1: 1, 3: 2}, res.BatchSizeHistogram)
	require.Equal(t, map[string]int{
		"land: commit <oid> not found": 2,
		"land: hook rejected":          1,
	}, res.FailureClasses)
	require.Equal(t, []string{
		"land: commit " + oidA + " not found",
		"land: hook rejected",
	}, res.Errors, "the first failure of each kind is kept for diagnosis")
}

func TestFormatHistogram(t *testing.T) {
	t.Parallel()

	require.Equal(t, "-", formatHistogram(nil))
	require.Equal(t, "-", formatHistogram(map[int]int{}))
	require.Equal(t, "1: 2, 3: 1, 10: 4", formatHistogram(map[int]int{10: 4, 1: 2, 3: 1}), "keys are numeric, not lexical")
}

func TestWriteResultShowsBatchingAndFailures(t *testing.T) {
	t.Parallel()

	res := Result{
		Label:              "batching-on-20",
		Mode:               ModeLandBatch,
		Changes:            3,
		Landed:             2,
		Failed:             1,
		AttemptsHistogram:  map[int]int{1: 2},
		BatchSizeHistogram: map[int]int{1: 1, 4: 1},
		FailureClasses:     map[string]int{"land: hook rejected": 1},
	}

	var out bytes.Buffer
	require.NoError(t, writeResult(&out, "table", res))
	text := out.String()
	require.Contains(t, text, "batching-on-20")
	require.Contains(t, text, "attempts histogram")
	require.Contains(t, text, "1: 2")
	require.Contains(t, text, "batch size histogram")
	require.Contains(t, text, "1: 1, 4: 1")
	require.Contains(t, text, "failure")
	require.Contains(t, text, "1 x land: hook rejected")

	var unlabelled bytes.Buffer
	require.NoError(t, writeResult(&unlabelled, "table", Result{Mode: ModeLand}))
	require.NotContains(t, unlabelled.String(), "label", "a run without --label prints no label row")
}

func TestWriteMarkdownReport(t *testing.T) {
	t.Parallel()

	res := Result{
		Label:              "batching-on-50",
		Mode:               ModeLandBatch,
		Agents:             50,
		Changes:            250,
		Landed:             250,
		AttemptsHistogram:  map[int]int{1: 240, 2: 10},
		BatchSizeHistogram: map[int]int{4: 250},
		P95MS:              12.5,
		FailureClasses:     map[string]int{"land: a|b": 2},
	}

	var out bytes.Buffer
	require.NoError(t, writeMarkdownReport(&out, res))
	text := out.String()
	require.True(t, strings.HasPrefix(text, "## graft-bench: batching-on-50\n\n| metric | batching-on-50 |\n"), text)
	require.Contains(t, text, "| attempts histogram (landed) | 1: 240, 2: 10 |")
	require.Contains(t, text, "| attempts histogram (failed) | - |")
	require.Contains(t, text, "| batch size histogram | 4: 250 |")
	require.Contains(t, text, "| latency p95 (landed) | 12.5ms |")
	require.Contains(t, text, "Failed changes by kind:")
	require.Contains(t, text, `- 2 x land: a\|b`)

	var unlabelled bytes.Buffer
	require.NoError(t, writeMarkdownReport(&unlabelled, Result{Mode: ModeLand}))
	require.True(t, strings.HasPrefix(unlabelled.String(), "## graft-bench: land\n"))
	require.NotContains(t, unlabelled.String(), "Failed changes by kind")
}

func TestWriteReportFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	res := Result{Label: "run-a", Mode: ModeLand, Landed: 3, AttemptsHistogram: map[int]int{1: 3}}

	require.NoError(t, writeReportFiles(res, "", ""))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "no paths, no files")

	jsonPath := filepath.Join(dir, "run.json")
	mdPath := filepath.Join(dir, "run.md")
	require.NoError(t, writeReportFiles(res, jsonPath, mdPath))

	loaded, err := loadResult(jsonPath)
	require.NoError(t, err)
	require.Equal(t, res, loaded, "the JSON file holds the whole result")

	md, err := os.ReadFile(mdPath)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(md), "## graft-bench: run-a\n"))

	require.Error(t, writeReportFiles(res, filepath.Join(dir, "missing", "run.json"), ""))
}

func TestCompareUsesLabels(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	off := Result{Label: "batching-off-50", Mode: ModeLand, Agents: 50, Landed: 240}
	on := Result{Label: "batching-on-50", Mode: ModeLandBatch, Agents: 50, Landed: 250}

	var paths []string
	for name, res := range map[string]Result{"off.json": off, "on.json": on} {
		path := filepath.Join(dir, name)
		require.NoError(t, writeReportFiles(res, path, ""))
		paths = append(paths, path)
	}

	var out, errOut bytes.Buffer
	require.Zero(t, run(context.Background(), append([]string{"compare", "--format", "markdown"}, paths...), &out, &errOut), errOut.String())
	header := strings.Split(out.String(), "\n")[0]
	require.Contains(t, header, "batching-off-50")
	require.Contains(t, header, "batching-on-50")
	require.NotContains(t, header, ".json", "a label replaces the file name")
}

func TestParseOptionsReportFlags(t *testing.T) {
	t.Parallel()

	opts, err := parseOptions([]string{
		"--address", "unix:/tmp/g.sock", "--repo", "r.git",
		"--label", "batching-on-20", "--json-out", "on.json", "--md-out", "on.md",
	}, &bytes.Buffer{})
	require.NoError(t, err)
	require.Equal(t, "batching-on-20", opts.label)
	require.Equal(t, "on.json", opts.jsonOut)
	require.Equal(t, "on.md", opts.mdOut)

	_, err = parseOptions([]string{
		"--address", "unix:/tmp/g.sock", "--repo", "r.git", "--label", "a|b",
	}, &bytes.Buffer{})
	require.ErrorContains(t, err, "--label must not contain")

	_, err = parseOptions([]string{
		"--address", "unix:/tmp/g.sock", "--repo", "r.git", "--label", "a\nb",
	}, &bytes.Buffer{})
	require.ErrorContains(t, err, "--label must not contain")
}

func TestFailedLandOutcome(t *testing.T) {
	t.Parallel()

	keptMoving := structerr.NewFailedPrecondition(
		"branch %q kept moving, giving up after %d attempts: %w", "bench", 20, errors.New("reference changed"),
	).WithDetail(&gitalypb.UserLandError{
		Error: &gitalypb.UserLandError_ReferenceUpdate{
			ReferenceUpdate: &gitalypb.ReferenceUpdateError{ReferenceName: []byte("refs/heads/bench")},
		},
	})
	mergeConflict := structerr.NewFailedPrecondition("landing commit: conflict").WithDetail(&gitalypb.UserLandError{
		Error: &gitalypb.UserLandError_MergeConflict{MergeConflict: &gitalypb.MergeConflictError{}},
	})

	t.Run("a land that kept losing the race reports its attempts", func(t *testing.T) {
		t.Parallel()

		require.Equal(t, LandOutcome{Attempts: 20}, failedLandOutcome(keptMoving))
	})

	t.Run("the message alone is not enough", func(t *testing.T) {
		t.Parallel()

		err := structerr.NewFailedPrecondition("giving up after 7 attempts")
		require.Equal(t, LandOutcome{}, failedLandOutcome(err), "without the reference update detail the count is not trusted")
	})

	t.Run("a merge conflict reports no attempts", func(t *testing.T) {
		t.Parallel()

		require.Equal(t, LandOutcome{Conflict: true}, failedLandOutcome(mergeConflict))
	})

	t.Run("the attempts reach the report", func(t *testing.T) {
		t.Parallel()

		// One change landed at the first attempt, and one gave up after 20
		// attempts. The server made 21 attempts for one landed change.
		samples := []Sample{
			{Landed: true, Outcome: LandOutcome{Attempts: 1, BatchSize: 1}},
			{Err: fmt.Errorf("land: %w", keptMoving), Outcome: failedLandOutcome(keptMoving)},
		}
		res := aggregate(baseConfig(), samples, time.Second)

		require.Equal(t, int64(21), res.Attempts)
		require.Equal(t, int32(20), res.MaxAttempts)
		require.InDelta(t, 21, res.AttemptsPerLand, 1e-9)
		require.Equal(t, map[int]int{1: 1}, res.AttemptsHistogram, "the landed histogram holds landed changes only")
		require.Equal(t, map[int]int{20: 1}, res.FailedAttemptsHistogram)

		var table bytes.Buffer
		require.NoError(t, writeResult(&table, "table", res))
		require.Contains(t, table.String(), "20: 1")

		var md bytes.Buffer
		require.NoError(t, writeMarkdownReport(&md, res))
		require.Contains(t, md.String(), "| attempts histogram (failed) | 20: 1 |")
		require.Contains(t, md.String(), "| max attempts (all changes) | 20 |")
	})
}
