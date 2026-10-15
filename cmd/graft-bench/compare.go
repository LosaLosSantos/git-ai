package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// comparisonMetric is one row of the comparison report.
type comparisonMetric struct {
	name  string
	value func(Result) string
}

// comparisonMetrics are the rows of the comparison report, one column per run.
var comparisonMetrics = []comparisonMetric{
	{"mode", func(r Result) string { return string(r.Mode) }},
	{"agents", func(r Result) string { return fmt.Sprint(r.Agents) }},
	{"branches", func(r Result) string { return fmt.Sprint(r.Branches) }},
	{"changes", func(r Result) string { return fmt.Sprint(r.Changes) }},
	{"landed", func(r Result) string { return fmt.Sprint(r.Landed) }},
	{"failed", func(r Result) string { return fmt.Sprint(r.Failed) }},
	{"conflict rate", func(r Result) string { return fmt.Sprintf("%.3f", r.ObservedConflictRate) }},
	// The attempts of failed changes are server work too: they are in the
	// total, the per-land mean and the maximum, and have their own histogram.
	{"attempts (all changes)", func(r Result) string { return fmt.Sprint(r.Attempts) }},
	{"attempts/landed change", func(r Result) string { return fmt.Sprintf("%.2f", r.AttemptsPerLand) }},
	{"max attempts (all changes)", func(r Result) string { return fmt.Sprint(r.MaxAttempts) }},
	{"attempts histogram (landed)", func(r Result) string { return formatHistogram(r.AttemptsHistogram) }},
	{"attempts histogram (failed)", func(r Result) string { return formatHistogram(r.FailedAttemptsHistogram) }},
	{"batch size mean", func(r Result) string { return fmt.Sprintf("%.2f", r.BatchSizeMean) }},
	{"batch size max", func(r Result) string { return fmt.Sprint(r.BatchSizeMax) }},
	{"batch size histogram", func(r Result) string { return formatHistogram(r.BatchSizeHistogram) }},
	{"duration", func(r Result) string { return fmt.Sprintf("%.2fs", r.DurationSeconds) }},
	{"lands/min", func(r Result) string { return fmt.Sprintf("%.1f", r.LandsPerMinute) }},
	{"latency p50 (landed)", func(r Result) string { return fmt.Sprintf("%.1fms", r.P50MS) }},
	{"latency p95 (landed)", func(r Result) string { return fmt.Sprintf("%.1fms", r.P95MS) }},
	{"latency p99 (landed)", func(r Result) string { return fmt.Sprintf("%.1fms", r.P99MS) }},
	{"latency p95 (failed)", func(r Result) string { return fmt.Sprintf("%.1fms", r.FailedP95MS) }},
}

// writeComparison prints the runs side by side, one column per run, labelled
// by labels. format is table or markdown.
func writeComparison(w io.Writer, format string, labels []string, results []Result) error {
	if len(labels) != len(results) {
		return fmt.Errorf("%d labels for %d results", len(labels), len(results))
	}
	if len(results) == 0 {
		return errors.New("nothing to compare")
	}
	if format != "table" && format != "markdown" {
		return fmt.Errorf("unknown format %q, want table or markdown", format)
	}

	header := append([]string{"metric"}, labels...)
	rows := make([][]string, 0, len(comparisonMetrics))
	for _, m := range comparisonMetrics {
		row := []string{m.name}
		for _, r := range results {
			row = append(row, m.value(r))
		}
		rows = append(rows, row)
	}

	if format == "markdown" {
		return writeMarkdownRows(w, header, rows)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, row := range append([][]string{header}, rows...) {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

// writeMarkdownReport writes res as a Markdown report: a heading naming the
// run, its metrics as a table, and the failed changes by kind.
func writeMarkdownReport(w io.Writer, res Result) error {
	title := res.Label
	if title == "" {
		title = string(res.Mode)
	}

	if _, err := fmt.Fprintf(w, "## graft-bench: %s\n\n", title); err != nil {
		return err
	}
	if err := writeComparison(w, "markdown", []string{title}, []Result{res}); err != nil {
		return err
	}
	if err := writeNodeMarkdown(w, res); err != nil {
		return err
	}
	if len(res.FailureClasses) == 0 {
		return nil
	}

	if _, err := fmt.Fprint(w, "\nFailed changes by kind:\n\n"); err != nil {
		return err
	}
	for _, class := range sortedKeys(res.FailureClasses) {
		// A | would end the cell of the message in other renderers.
		msg := strings.ReplaceAll(class, "|", `\|`)
		if _, err := fmt.Fprintf(w, "- %d x %s\n", res.FailureClasses[class], msg); err != nil {
			return err
		}
	}

	return nil
}

// writeMarkdownRows writes header and rows as a Markdown pipe table.
func writeMarkdownRows(w io.Writer, header []string, rows [][]string) error {
	line := func(cells []string) string {
		return "| " + strings.Join(cells, " | ") + " |\n"
	}

	sep := make([]string, len(header))
	for i := range sep {
		sep[i] = "---"
	}

	var b strings.Builder
	b.WriteString(line(header))
	b.WriteString(line(sep))
	for _, row := range rows {
		b.WriteString(line(row))
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// runCompare implements "graft-bench compare [--format f] result.json...". It
// prints a comparison of saved --format json results, one column per file,
// and returns the process exit code.
func runCompare(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("graft-bench compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "table", "output format: table or markdown")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "graft-bench: compare needs at least one result file (saved with --format json)")
		return 2
	}

	labels := make([]string, 0, fs.NArg())
	results := make([]Result, 0, fs.NArg())
	for _, path := range fs.Args() {
		res, err := loadResult(path)
		if err != nil {
			fmt.Fprintf(stderr, "graft-bench: %v\n", err)
			return 1
		}
		label := res.Label
		if label == "" {
			label = filepath.Base(path)
		}
		labels = append(labels, label)
		results = append(results, res)
	}

	if err := writeComparison(stdout, *format, labels, results); err != nil {
		fmt.Fprintf(stderr, "graft-bench: %v\n", err)
		return 2
	}

	return 0
}

// loadResult reads a result saved with --format json.
func loadResult(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}

	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		return Result{}, fmt.Errorf("read %s: %w", path, err)
	}

	return res, nil
}
