package chat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSelectSourcesByKeyword(t *testing.T) {
	all := []string{sourceChangesets, sourceVerify, sourceMetrics, sourceLogs}
	tests := []struct {
		question string
		want     []string
	}{
		{"why did the build fail?", []string{sourceVerify, sourceLogs}},
		{"how many changes landed?", []string{sourceChangesets, sourceMetrics}},
		{"hello there", all},
	}
	for _, tc := range tests {
		got := selectSources(all, tc.question)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("selectSources(%q) = %v, want %v", tc.question, got, tc.want)
		}
	}
}

func TestSummarizeChangesets(t *testing.T) {
	if got := summarizeChangesets([]byte(`[{"id":1},{"id":2}]`), builtinRedactor(t)); !strings.HasPrefix(got, "2 changesets") {
		t.Errorf("JSON array summary = %q", got)
	}
	if got := summarizeChangesets([]byte("a\nb\n\nc\n"), builtinRedactor(t)); !strings.HasPrefix(got, "3 lines of changeset output") {
		t.Errorf("line summary = %q", got)
	}
}

func TestChangesetsRunnerRedactsAndBounds(t *testing.T) {
	var gotArgv []string
	var gotDir string
	run := func(_ context.Context, argv []string, dir string) ([]byte, error) {
		gotArgv, gotDir = argv, dir
		return []byte(`[{"id":"7","subject":"fix password=hunter2 handling"}]`), nil
	}
	cfg := &Config{Sources: SourcesConfig{Changesets: ChangesetsConfig{Argv: []string{"graft", "log", "--json"}, Dir: "/work"}}}
	e := newTestEngine(t, cfg, nil, run)

	sections := e.gather.Collect(context.Background(), "what changed?")
	if len(sections) != 1 || sections[0].Name != sourceChangesets {
		t.Fatalf("sections = %+v", sections)
	}
	if strings.Join(gotArgv, " ") != "graft log --json" || gotDir != "/work" {
		t.Fatalf("runner got %v in %q", gotArgv, gotDir)
	}
	if strings.Contains(sections[0].Body, "hunter2") || !strings.Contains(sections[0].Body, "[REDACTED]") {
		t.Fatalf("body not redacted: %s", sections[0].Body)
	}
	if !strings.HasPrefix(sections[0].Summary, "1 changesets") {
		t.Fatalf("summary = %q", sections[0].Summary)
	}
}

func TestSourceFailureBecomesSection(t *testing.T) {
	run := func(context.Context, []string, string) ([]byte, error) {
		return nil, errors.New("command graft failed: exit status 2")
	}
	cfg := &Config{Sources: SourcesConfig{Changesets: ChangesetsConfig{Argv: []string{"graft"}}}}
	e := newTestEngine(t, cfg, nil, run)

	sections := e.gather.Collect(context.Background(), "what changed?")
	if len(sections) != 1 || !strings.HasPrefix(sections[0].Body, "unavailable: ") {
		t.Fatalf("sections = %+v", sections)
	}
}

func TestMetricsKeepsOnlyGraftLines(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "# HELP go_goroutines Number of goroutines.\n"+
			"go_goroutines 42\n"+
			"# HELP graft_changesets_total Changesets seen.\n"+
			"# TYPE graft_changesets_total counter\n"+
			"graft_changesets_total{component=\"api\"} 7\n")
	}))
	t.Cleanup(hs.Close)

	cfg := &Config{Sources: SourcesConfig{MetricsURL: hs.URL + "/metrics"}}
	e := newTestEngine(t, cfg, nil, nil)
	sections := e.gather.Collect(context.Background(), "how many changesets?")
	if len(sections) != 1 {
		t.Fatalf("sections = %+v", sections)
	}
	body := sections[0].Body
	if !strings.Contains(body, "graft_changesets_total") || strings.Contains(body, "go_goroutines") {
		t.Fatalf("metrics body = %q", body)
	}
}

func TestMetricsHTTPErrorBecomesSection(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(hs.Close)

	cfg := &Config{Sources: SourcesConfig{MetricsURL: hs.URL}}
	e := newTestEngine(t, cfg, nil, nil)
	sections := e.gather.Collect(context.Background(), "how many?")
	if len(sections) != 1 || !strings.Contains(sections[0].Body, "HTTP 500") {
		t.Fatalf("sections = %+v", sections)
	}
}

func TestTailLinesReturnsLastLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	path := writeFile(t, "small.log", b.String())

	got, err := tailLines(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "line 497|line 498|line 499" {
		t.Fatalf("tailLines = %v", got)
	}
}

func TestTailLinesOfLargeFileDropsCutLine(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, "line %06d padding padding padding\n", i)
	}
	path := writeFile(t, "large.log", b.String())

	got, err := tailLines(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != "line 019999 padding padding padding" || !strings.HasPrefix(got[0], "line 0") {
		t.Fatalf("tailLines = %q", got)
	}
	if !strings.HasPrefix(got[0], "line 019998 ") {
		t.Fatalf("first tailed line is cut: %q", got[0])
	}
}

func TestLogsSectionRedactsAndHandlesMissingFiles(t *testing.T) {
	good := writeFile(t, "app.log", "started\nlogin password=hunter2\n")
	missing := filepath.Join(t.TempDir(), "gone.log")
	cfg := &Config{Sources: SourcesConfig{LogFiles: []string{good, missing}}}
	e := newTestEngine(t, cfg, nil, nil)

	sections := e.gather.Collect(context.Background(), "why does it crash?")
	if len(sections) != 1 {
		t.Fatalf("sections = %+v", sections)
	}
	body := sections[0].Body
	if strings.Contains(body, "hunter2") || !strings.Contains(body, "login password=[REDACTED]") {
		t.Fatalf("log body not redacted: %q", body)
	}
	if !strings.Contains(body, "gone.log: unavailable") {
		t.Fatalf("missing file not reported: %q", body)
	}
}

func TestContextIsBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "error line %d with some words in it\n", i)
	}
	logs := writeFile(t, "big.log", b.String())
	verify := writeFile(t, "verify.txt", strings.Repeat("state: failing\n", 500))
	cfg := &Config{
		MaxContextBytes: 1024,
		Sources: SourcesConfig{
			VerifyState:  verify,
			LogFiles:     []string{logs},
			LogTailLines: 2000,
		},
	}
	e := newTestEngine(t, cfg, nil, nil)

	sections := e.gather.Collect(context.Background(), "why is the state failing with errors?")
	if len(sections) != 2 {
		t.Fatalf("sections = %d, want 2", len(sections))
	}
	rendered := renderContext(sections, "0123456789abcdef01234567")
	if len(rendered) > 1024 {
		t.Fatalf("rendered context is %d bytes, budget 1024", len(rendered))
	}
	if !strings.Contains(rendered, "END CONTEXT 0123456789abcdef01234567") {
		t.Fatalf("END delimiter lost: %q", rendered[len(rendered)-80:])
	}
	if !utf8.ValidString(rendered) {
		t.Fatal("rendered context is not valid UTF-8")
	}
}

func TestTruncateUTF8(t *testing.T) {
	in := "héllo wörld, ça va très bien"
	for limit := 0; limit <= len(in)+2; limit++ {
		out := truncateUTF8(in, limit)
		if len(out) > limit {
			t.Fatalf("limit %d: got %d bytes", limit, len(out))
		}
		if !utf8.ValidString(out) {
			t.Fatalf("limit %d: invalid UTF-8 %q", limit, out)
		}
	}
	if got := truncateUTF8("short", 100); got != "short" {
		t.Fatalf("short string changed: %q", got)
	}
}

func TestChangesetsFileSource(t *testing.T) {
	path := writeFile(t, "log.json", `[{"id":"a"},{"id":"b"}]`)
	cfg := &Config{Sources: SourcesConfig{Changesets: ChangesetsConfig{File: path}}}
	e := newTestEngine(t, cfg, nil, nil)
	sections := e.gather.Collect(context.Background(), "what landed?")
	if len(sections) != 1 || !strings.Contains(sections[0].Body, `"id":"b"`) {
		t.Fatalf("sections = %+v", sections)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
