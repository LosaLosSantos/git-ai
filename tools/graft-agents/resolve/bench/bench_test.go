package bench

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
)

// fixture is a repository with crafted merges.
type fixture struct {
	t   *testing.T
	dir string
	now int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	f := &fixture{t: t, dir: t.TempDir(), now: 1767225600}
	f.git("init", "-q", "-b", "main")
	return f
}

// git runs git in the fixture with a fixed identity and date, given on the command line so that no
// configuration file is touched.
func (f *fixture) git(args ...string) string {
	f.t.Helper()
	f.now += 60
	cmd := exec.Command("git", append([]string{"-C", f.dir, "-c", "user.name=Bench", "-c", "user.email=bench@example.com",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	date := time.Unix(f.now, 0).UTC().Format(time.RFC3339)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(name, content string) {
	f.t.Helper()
	p := filepath.Join(f.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) commit(msg string, files map[string]string) string {
	f.t.Helper()
	for name, content := range files {
		f.write(name, content)
	}
	f.git("add", "-A")
	f.git("commit", "-q", "-m", msg)
	return f.git("rev-parse", "HEAD")
}

// conflictMerge merges branch into the current branch, which conflicts, and commits the human
// resolution.
func (f *fixture) conflictMerge(branch string, resolved map[string]string) string {
	f.t.Helper()
	cmd := exec.Command("git", "-C", f.dir, "-c", "user.name=Bench", "-c", "user.email=bench@example.com", "merge", "--no-ff", "-q", branch)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err == nil {
		f.t.Fatalf("merge of %s did not conflict: %s", branch, out)
	}
	return f.commit("Merge "+branch, resolved)
}

const (
	goBase   = "package a\n\nfunc A() int {\n\treturn 1\n}\n"
	goOurs   = "package a\n\nfunc A() int {\n\treturn 2\n}\n"
	goTheirs = "package a\n\nfunc A() int {\n\treturn 3\n}\n"
	goHuman  = "package a\n\nfunc A() int {\n\treturn 2 + 3\n}\n"
)

// buildHistory creates: a conflicting merge of a.go resolved by hand (one case), a conflicting
// merge of b.py where the human took one side (not a case), a conflict in notes.txt (no family),
// and a clean merge.
func buildHistory(f *fixture) (mergeA string) {
	f.commit("base", map[string]string{"a.go": goBase, "b.py": "x = 1\n", "notes.txt": "one\n"})

	f.git("checkout", "-q", "-b", "feature")
	f.commit("feature: A returns 3", map[string]string{"a.go": goTheirs})
	f.git("checkout", "-q", "main")
	f.commit("main: A returns 2", map[string]string{"a.go": goOurs})
	mergeA = f.conflictMerge("feature", map[string]string{"a.go": goHuman})

	f.git("checkout", "-q", "-b", "feature2")
	f.commit("feature2", map[string]string{"b.py": "x = 3\n", "notes.txt": "three\n"})
	f.git("checkout", "-q", "main")
	f.commit("main2", map[string]string{"b.py": "x = 2\n", "notes.txt": "two\n"})
	f.conflictMerge("feature2", map[string]string{"b.py": "x = 2\n", "notes.txt": "two and three\n"})

	f.git("checkout", "-q", "-b", "feature3")
	f.commit("feature3", map[string]string{"c.go": "package c\n"})
	f.git("checkout", "-q", "main")
	f.commit("main3", map[string]string{"d.go": "package d\n"})
	f.git("merge", "--no-ff", "-q", "-m", "clean merge", "feature3")
	return mergeA
}

func TestMineKeepsRealResolutions(t *testing.T) {
	f := newFixture(t)
	mergeA := buildHistory(f)
	head := f.git("rev-parse", "HEAD")

	cases, stats, err := Mine(context.Background(), Repo{Name: "fixture", Commit: head, License: "MIT"}, f.dir, MineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("got %d cases, want 1: %+v (stats %+v)", len(cases), cases, stats)
	}
	c := cases[0]
	if c.Merge != mergeA || c.Path != "a.go" || c.Family != FamilyGo || c.Kind != protocol.KindContent || c.License != "MIT" {
		t.Fatalf("unexpected case %+v", c)
	}
	if c.BaseBlob == nil || c.OursBlob == nil || c.TheirsBlob == nil || c.HumanBlob == nil {
		t.Fatalf("missing sides in %+v", c)
	}
	want := MineStats{Merges: 3, ConflictingMerges: 2, ConflictingFiles: 3, OtherFamily: 1, OneSide: 1, Kept: 1}
	if stats != want {
		t.Fatalf("stats %+v, want %+v", stats, want)
	}

	// The manifest replays identically.
	file := filepath.Join(t.TempDir(), "manifest.json")
	if err := SaveManifest(file, Manifest{Version: ManifestVersion, Cases: cases}); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Cases) != 1 || m.Cases[0].ID != c.ID || *m.Cases[0].HumanBlob != *c.HumanBlob {
		t.Fatalf("manifest round trip changed the case: %+v", m.Cases)
	}
}

// TestMineSkipsUnrelatedHistories mines a repository that absorbed another one with a merge of
// unrelated histories, which git merge-tree refuses: the merge is skipped and counted, and the
// conflicting merge after it is still mined.
func TestMineSkipsUnrelatedHistories(t *testing.T) {
	f := newFixture(t)
	f.commit("base", map[string]string{"a.go": "package a\n\nvar x = 1\n"})
	f.git("checkout", "-q", "--orphan", "other")
	f.git("rm", "-q", "-r", "--cached", ".")
	f.commit("other root", map[string]string{"b.py": "y = 1\n"})
	f.git("checkout", "-q", "-f", "main")
	f.git("merge", "-q", "--no-ff", "--allow-unrelated-histories", "-m", "absorb other", "other")

	f.git("checkout", "-q", "-b", "feature")
	f.commit("three", map[string]string{"a.go": "package a\n\nvar x = 3\n"})
	f.git("checkout", "-q", "main")
	f.commit("two", map[string]string{"a.go": "package a\n\nvar x = 2\n"})
	f.conflictMerge("feature", map[string]string{"a.go": "package a\n\nvar x = 2 + 3\n"})
	head := f.git("rev-parse", "HEAD")

	cases, stats, err := Mine(context.Background(), Repo{Name: "fixture", Commit: head, License: "MIT"}, f.dir, MineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := MineStats{Merges: 2, ConflictingMerges: 1, SkippedMergeBases: 1, ConflictingFiles: 1, Kept: 1}
	if stats != want || len(cases) != 1 {
		t.Fatalf("stats %+v (%d cases), want %+v and 1 case", stats, len(cases), want)
	}
}

// fixedStrategy answers every file with content, or fails with err.
type fixedStrategy struct {
	content []byte
	err     error
}

func (s fixedStrategy) Resolve(_ context.Context, _ resolve.File, _ protocol.Request) (resolve.Resolution, bool, error) {
	if s.err != nil {
		return resolve.Resolution{}, false, s.err
	}
	return resolve.Resolution{Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: s.content}, true, nil
}

func TestRunnerOutcomes(t *testing.T) {
	f := newFixture(t)
	buildHistory(f)
	head := f.git("rev-parse", "HEAD")
	cases, _, err := Mine(context.Background(), Repo{Name: "fixture", Commit: head, License: "MIT"}, f.dir, MineOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		strategy resolve.Strategy
		want     string
		exact    bool
	}{
		{"correct", fixedStrategy{content: []byte(goHuman)}, OutcomeCorrect, true},
		{"correct after normalizing", fixedStrategy{content: []byte(strings.ReplaceAll(goHuman, "\n", "  \r\n") + "\n\n")}, OutcomeCorrect, false},
		{"wrong", fixedStrategy{content: []byte(goOurs)}, OutcomeWrong, false},
		{"rejected by the marker rule", fixedStrategy{content: []byte("<<<<<<< ours\nx\n=======\ny\n>>>>>>> theirs\n")}, OutcomeRejected, false},
		{"model error", fixedStrategy{err: errors.New("provider: 500")}, OutcomeModelError, false},
		{"budget", fixedStrategy{err: resolve.ErrBudgetExhausted}, OutcomeNotRun, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := resolve.NewResolver([]resolve.Binding{{Name: "fake", Strategy: tc.strategy}}, nil, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			results, err := Runner{Resolver: res, Clone: func(string) string { return f.dir }}.Run(context.Background(), cases)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Outcome != tc.want || results[0].Exact != tc.exact {
				t.Fatalf("results %+v, want outcome %s exact %v", results, tc.want, tc.exact)
			}
		})
	}
}

func TestNotSentReason(t *testing.T) {
	big := strings.Repeat("x", 3000)
	for _, tc := range []struct {
		path      string
		side      string
		generated bool
		want      string
	}{
		{"a.go", "package a\n", false, ""},
		{"a.bin", "a\x00b", false, NotSentBinary},
		{"big.go", strings.Repeat("a\n", 600), false, NotSentLarge},
		{"web/package-lock.json", "{}", false, NotSentLockfile},
		{"go.sum", "x\n", false, NotSentLockfile},
		{"gen.go", "// Code generated by protoc-gen-go. DO NOT EDIT.\npackage gen\n", false, NotSentGenerated},
		{"x.ts", "// @generated\n", false, NotSentGenerated},
		{"y.py", "x = 1\n", true, NotSentGenerated},
		{"app.min.js", "a\n", false, NotSentMinified},
		{"bundle.js", big + "\n", false, NotSentMinified},
		{"code.js", strings.Repeat("let a = 1;\n", 400), false, ""},
	} {
		maxBytes := int64(protocol.DefaultMaxFileBytes)
		if tc.path == "big.go" {
			maxBytes = 100
		}
		got := notSentReason(tc.path, [][]byte{[]byte(tc.side)}, maxBytes, tc.generated)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestSampleIsStratifiedAndStable(t *testing.T) {
	var cases []Case
	for i := 0; i < 30; i++ {
		family := []string{FamilyGo, FamilyPython, FamilyTSJS}[i%3]
		cases = append(cases, Case{ID: string(rune('a'+i)) + family, Family: family})
	}
	a, b := Sample(cases, 9, 7), Sample(cases, 9, 7)
	counts := map[string]int{}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("the sample is not stable: %v vs %v", a, b)
		}
		counts[a[i].Family]++
	}
	if counts[FamilyGo] != 3 || counts[FamilyPython] != 3 || counts[FamilyTSJS] != 3 {
		t.Fatalf("the sample is not stratified: %v", counts)
	}
	if c := Sample(cases, 9, 8); c[0].ID == a[0].ID && c[1].ID == a[1].ID && c[2].ID == a[2].ID {
		t.Fatalf("another seed gave the same sample")
	}
	if len(Sample(cases, 0, 1)) != 30 {
		t.Fatal("n <= 0 must take every case")
	}
}

func TestReportCounts(t *testing.T) {
	rep := NewReport("test", []Result{
		{Family: FamilyGo, Outcome: OutcomeCorrect, Exact: true},
		{Family: FamilyGo, Outcome: OutcomeWrong},
		{Family: FamilyPython, Outcome: OutcomeRejected, HumanSyntaxError: "syntax check: a.py does not parse"},
		{Family: FamilyPython, Outcome: OutcomeNotSent, Reason: NotSentGenerated},
	})
	if rep.Total.HumanSyntax != 1 || rep.Families[FamilyPython].HumanSyntax != 1 {
		t.Fatalf("human syntax failures %+v", rep.Families)
	}
	if rep.Total.Cases != 4 || rep.Total.Accepted() != 2 || rep.Total.ErrorRate() != 0.5 {
		t.Fatalf("totals %+v", rep.Total)
	}
	if rep.Families[FamilyGo].Correct != 1 || rep.NotSent[NotSentGenerated] != 1 {
		t.Fatalf("families %+v, not sent %+v", rep.Families, rep.NotSent)
	}
	var b strings.Builder
	if err := rep.WriteMarkdown(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "| total | 4 | 1 (1) | 1 | 1 | 1 |") || !strings.Contains(b.String(), "reject the right answer: 1 (go 0, python 1)") {
		t.Fatalf("markdown:\n%s", b.String())
	}
}

// TestCommittedCorpusLoads checks the corpus file of the benchmark: permissive licenses, no
// duplicate, a pinned commit for every repository.
func TestCommittedCorpusLoads(t *testing.T) {
	c, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range c.Repos {
		if len(r.Commit) != 40 {
			t.Errorf("%s: commit %q is not a full object ID", r.Name, r.Commit)
		}
	}
}
