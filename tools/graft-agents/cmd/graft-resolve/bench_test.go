package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/bench"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/syntax"
)

// gitIn runs git in dir with a fixed identity, given on the command line.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Bench", "-c", "user.email=bench@example.com",
		"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil && !(len(args) > 0 && args[0] == "merge") {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestBenchMineAndRun mines a repository with one hand-resolved conflict through a file:// clone
// and runs the default chain (trivial only) on it, which abstains.
func TestBenchMineAndRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(src, "a.py"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, src, "add", "-A")
	}
	write("x = 1\n")
	gitIn(t, src, "commit", "-q", "-m", "base")
	gitIn(t, src, "checkout", "-q", "-b", "feature")
	write("x = 3\n")
	gitIn(t, src, "commit", "-q", "-m", "three")
	gitIn(t, src, "checkout", "-q", "main")
	write("x = 2\n")
	gitIn(t, src, "commit", "-q", "-m", "two")
	gitIn(t, src, "merge", "--no-ff", "-q", "feature")
	write("x = 2 + 3\n")
	gitIn(t, src, "commit", "-q", "-m", "merge feature")
	head := gitIn(t, src, "rev-parse", "HEAD")

	work := t.TempDir()
	corpus := filepath.Join(work, "corpus.json")
	data, _ := json.Marshal(bench.Corpus{Repos: []bench.Repo{{Name: "local/fixture", URL: "file://" + src, Commit: head, License: "MIT"}}})
	if err := os.WriteFile(corpus, data, 0o644); err != nil {
		t.Fatal(err)
	}
	clones, manifest := filepath.Join(work, "clones"), filepath.Join(work, "manifest.json")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"bench", "mine", "--corpus", corpus, "--clones", clones, "--manifest", manifest}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("mine exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 cases in") {
		t.Fatalf("mine output: %s", stdout.String())
	}

	// A second run takes the repository from its part, without the clone.
	moved := filepath.Join(work, "moved")
	if err := os.Rename(clones, moved); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := run([]string{"bench", "mine", "--corpus", corpus, "--clones", clones, "--manifest", manifest}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("resumed mine exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 cases in") {
		t.Fatalf("resumed mine output: %s", stdout.String())
	}
	if _, err := os.Stat(clones); !os.IsNotExist(err) {
		t.Fatalf("resumed mine cloned again: %v", err)
	}
	if err := os.Rename(moved, clones); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if !syntax.Available {
		// Without cgo the syntax check would reject every answer: the run refuses to start.
		if code := run([]string{"bench", "run", "--manifest", manifest, "--clones", clones}, nil, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "without cgo") {
			t.Fatalf("run without cgo exited %d: %s", code, stderr.String())
		}
		return
	}
	jsonOut := filepath.Join(work, "report.json")
	if code := run([]string{"bench", "run", "--manifest", manifest, "--clones", clones, "--label", "trivial", "--json-out", jsonOut}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("run exited %d: %s", code, stderr.String())
	}
	var report bench.Report
	raw, err := os.ReadFile(jsonOut)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Total.Cases != 1 || report.Total.Abstained != 1 || report.Results[0].Family != bench.FamilyPython {
		t.Fatalf("report %+v", report)
	}
	if !strings.Contains(stdout.String(), "| total | 1 |") {
		t.Fatalf("markdown: %s", stdout.String())
	}
}
