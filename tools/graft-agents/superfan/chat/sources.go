package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Source names, in the order their sections appear in the context.
const (
	sourceChangesets = "changesets"
	sourceVerify     = "verify"
	sourceMetrics    = "metrics"
	sourceLogs       = "logs"
)

var sourceOrder = []string{sourceChangesets, sourceVerify, sourceMetrics, sourceLogs}

// sourceKeywords select a source when one of its keywords appears in the
// question (matched in lower case). When no keyword matches, every configured
// source is used.
var sourceKeywords = map[string][]string{
	sourceChangesets: {"change", "commit", "land", "merge", "push", "recent", "log", "author", "who", "review", "branch", "changeset", "release", "deploy"},
	sourceVerify:     {"verif", "status", "state", "pass", "fail", "green", "broken", "health", "check"},
	sourceMetrics:    {"metric", "latency", "rate", "error", "slow", "perf", "throughput", "hit", "count", "how many", "prometheus", "percent", "queue", "%"},
	sourceLogs:       {"log", "error", "panic", "crash", "warn", "exception", "trace", "tail", "why", "stack"},
}

const (
	maxSourceBytes   = 1 << 20
	maxMetricsBytes  = 4 << 20
	metricsTimeout   = 5 * time.Second
	gatherTimeout    = 15 * time.Second
	commandWaitDelay = 5 * time.Second
	// tailWindow is how much of a log file is read from its end.
	tailWindow       = 256 << 10
	summaryItems     = 5
	summaryItemBytes = 160
	// sectionOverhead and contextOverhead reserve room for the headers of the
	// rendered context, so that the rendered context stays within its budget.
	sectionOverhead  = 128
	contextOverhead  = 256
	truncationMarker = "\n[truncated]"
	maxErrorBytes    = 200
	commandStderrCap = 4096
)

// Section is what one source contributes to an answer.
type Section struct {
	Name string
	// Summary is a deterministic description of the source, used when no
	// language model is configured.
	Summary string
	// Body is the redacted text of the source, bounded to the context budget.
	Body string
}

// CommandRunner runs argv without a shell in dir and returns its standard
// output. It is injected in tests.
type CommandRunner func(ctx context.Context, argv []string, dir string) ([]byte, error)

// ExecRunner is the production CommandRunner. Its standard output is bounded.
func ExecRunner(ctx context.Context, argv []string, dir string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	stdout := &limitedWriter{max: maxSourceBytes}
	cmd.Stdout = stdout
	cmd.Stderr = &limitedWriter{max: commandStderrCap}
	cmd.WaitDelay = commandWaitDelay
	if err := cmd.Run(); err != nil {
		// The error names the program only: its arguments and stderr may hold
		// data that must not reach the model or the log.
		return nil, fmt.Errorf("command %s failed: %w", filepath.Base(argv[0]), err)
	}
	return stdout.buf.Bytes(), nil
}

// limitedWriter keeps the first max bytes written and discards the rest.
type limitedWriter struct {
	buf bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

// gatherer collects the sections of the configured sources.
type gatherer struct {
	cfg    *Config
	red    *redactor
	run    CommandRunner
	client *http.Client
	budget int
}

// configured returns the sources the configuration enables, in sourceOrder.
func (g *gatherer) configured() []string {
	var out []string
	s := g.cfg.Sources
	if len(s.Changesets.Argv) > 0 || s.Changesets.File != "" {
		out = append(out, sourceChangesets)
	}
	if s.VerifyState != "" {
		out = append(out, sourceVerify)
	}
	if s.MetricsURL != "" {
		out = append(out, sourceMetrics)
	}
	if len(s.LogFiles) > 0 {
		out = append(out, sourceLogs)
	}
	return out
}

// selectSources returns the configured sources whose keywords appear in the
// question, or all configured sources when none matches.
func selectSources(configured []string, question string) []string {
	q := strings.ToLower(question)
	var picked []string
	for _, name := range configured {
		for _, kw := range sourceKeywords[name] {
			if strings.Contains(q, kw) {
				picked = append(picked, name)
				break
			}
		}
	}
	if len(picked) == 0 {
		return configured
	}
	return picked
}

// Collect gathers the sections for question. The total size of the bodies is
// bounded by the context budget. Every body is redacted.
func (g *gatherer) Collect(ctx context.Context, question string) []Section {
	names := selectSources(g.configured(), question)
	if len(names) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, gatherTimeout)
	defer cancel()
	sections := make([]Section, 0, len(names))
	for _, name := range names {
		sections = append(sections, g.fetch(ctx, name))
	}
	return boundSections(sections, g.budget)
}

// fetch reads one source. A failing source becomes a short "unavailable"
// section instead of failing the answer.
func (g *gatherer) fetch(ctx context.Context, name string) Section {
	var body, summary string
	var err error
	switch name {
	case sourceChangesets:
		body, summary, err = g.changesets(ctx)
	case sourceVerify:
		body, summary, err = g.verify()
	case sourceMetrics:
		body, summary, err = g.metrics(ctx)
	default:
		body, summary, err = g.logs()
	}
	if err != nil {
		msg := "unavailable: " + truncateUTF8(g.red.Apply(err.Error()), maxErrorBytes)
		return Section{Name: name, Summary: msg, Body: msg}
	}
	return Section{Name: name, Summary: g.red.Apply(summary), Body: g.red.Apply(body)}
}

func (g *gatherer) changesets(ctx context.Context) (string, string, error) {
	c := g.cfg.Sources.Changesets
	var data []byte
	var err error
	if len(c.Argv) > 0 {
		data, err = g.run(ctx, c.Argv, c.Dir)
	} else {
		data, err = readBounded(c.File)
	}
	if err != nil {
		return "", "", err
	}
	return string(data), summarizeChangesets(data, g.red), nil
}

// summarizeChangesets describes the changeset log: a JSON array is counted by
// element, anything else by line.
func summarizeChangesets(data []byte, red *redactor) string {
	var arr []json.RawMessage
	var head string
	var items []string
	if json.Unmarshal(data, &arr) == nil {
		head = fmt.Sprintf("%d changesets", len(arr))
		for _, raw := range arr {
			items = append(items, compactJSON(raw))
		}
	} else {
		lines := nonEmptyLines(string(data))
		head = fmt.Sprintf("%d lines of changeset output", len(lines))
		items = lines
	}
	return head + previewItems(items, red)
}

func (g *gatherer) verify() (string, string, error) {
	data, err := readBounded(g.cfg.Sources.VerifyState)
	if err != nil {
		return "", "", err
	}
	lines := nonEmptyLines(string(data))
	return string(data), fmt.Sprintf("%d lines of verification state", len(lines)) + previewItems(lines, g.red), nil
}

func (g *gatherer) metrics(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, metricsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.Sources.MetricsURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("metrics request: %w", safeErr(err))
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("scrape metrics: %w", safeErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("scrape metrics: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMetricsBytes))
	if err != nil {
		return "", "", fmt.Errorf("scrape metrics: read: %w", err)
	}
	var keep []string
	for _, line := range nonEmptyLines(string(data)) {
		if isGraftMetricLine(line) {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n"), fmt.Sprintf("%d graft_ metric lines", len(keep)) + previewItems(keep, g.red), nil
}

// isGraftMetricLine keeps the samples and the HELP and TYPE comments of graft_
// metrics.
func isGraftMetricLine(line string) bool {
	return strings.HasPrefix(line, "graft_") ||
		strings.HasPrefix(line, "# HELP graft_") ||
		strings.HasPrefix(line, "# TYPE graft_")
}

func (g *gatherer) logs() (string, string, error) {
	n := g.cfg.Sources.tailCount()
	var b strings.Builder
	total := 0
	for _, path := range g.cfg.Sources.LogFiles {
		name := filepath.Base(path)
		lines, err := tailLines(path, n)
		if err != nil {
			fmt.Fprintf(&b, "--- %s: unavailable\n", name)
			continue
		}
		total += len(lines)
		fmt.Fprintf(&b, "--- %s (last %d lines)\n", name, len(lines))
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	summary := fmt.Sprintf("%d log files, %d lines tailed", len(g.cfg.Sources.LogFiles), total)
	return b.String(), summary, nil
}

// tailLines returns the last n non-empty lines of the file at path. Only the
// last tailWindow bytes are read; a line cut by that window is dropped.
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var start int64
	if st.Size() > tailWindow {
		start = st.Size() - tailWindow
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, tailWindow))
	if err != nil {
		return nil, err
	}
	text := string(data)
	if start > 0 {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			return nil, nil
		}
		text = text[i+1:]
	}
	lines := nonEmptyLines(text)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// readBounded reads at most maxSourceBytes of the file at path.
func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxSourceBytes))
}

// boundSections shortens the bodies so that the rendered context fits budget:
// every section gets an equal share of what the headers leave over.
func boundSections(sections []Section, budget int) []Section {
	if len(sections) == 0 {
		return sections
	}
	avail := max(budget-contextOverhead-len(sections)*sectionOverhead, 0)
	share := avail / len(sections)
	for i := range sections {
		sections[i].Body = truncateUTF8(sections[i].Body, share)
	}
	return sections
}

// renderContext delimits the sections with nonce-marked BEGIN and END lines, so
// that data cannot close the block early.
func renderContext(sections []Section, nonce string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "===== BEGIN CONTEXT %s =====\n", nonce)
	for _, s := range sections {
		fmt.Fprintf(&b, "--- source: %s ---\n%s\n", s.Name, s.Body)
	}
	fmt.Fprintf(&b, "===== END CONTEXT %s =====\n", nonce)
	return b.String()
}

// previewItems renders the first summaryItems items as indented bullets. Each
// item is redacted before it is truncated: a cut secret would no longer match
// its pattern, and a partial prefix would survive.
func previewItems(items []string, red *redactor) string {
	var b strings.Builder
	for i, it := range items {
		if i == summaryItems {
			b.WriteString("\n  ...")
			break
		}
		b.WriteString("\n  - " + truncateUTF8(red.Apply(it), summaryItemBytes))
	}
	return b.String()
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// nonEmptyLines splits s into lines and drops the blank ones.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// truncateUTF8 returns s within limit bytes, cut on a rune boundary and marked
// as truncated.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= len(truncationMarker) {
		return ""
	}
	cut := limit - len(truncationMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncationMarker
}

// safeErr drops the URL from a client error: a Slack response_url or a metrics
// URL may carry credentials.
func safeErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
