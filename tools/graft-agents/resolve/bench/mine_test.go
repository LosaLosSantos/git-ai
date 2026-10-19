//go:build cgo

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

// TestMineLeavesOutInvalidReferences mines two conflicting merges whose human resolutions are not
// valid references: one committed with the conflict markers, one that breaks the Go syntax although
// both sides parse. Neither is kept, and both are counted.
func TestMineLeavesOutInvalidReferences(t *testing.T) {
	f := newFixture(t)
	f.commit("base", map[string]string{"a.go": goBase, "b.go": goBase})
	f.git("checkout", "-q", "-b", "feature")
	f.commit("feature", map[string]string{"a.go": goTheirs, "b.go": goTheirs})
	f.git("checkout", "-q", "main")
	f.commit("main", map[string]string{"a.go": goOurs, "b.go": goOurs})
	f.conflictMerge("feature", map[string]string{
		"a.go": "package a\n\n<<<<<<< HEAD\nfunc A() int { return 2 }\n=======\nfunc A() int { return 3 }\n>>>>>>> feature\n",
		"b.go": "package a\n\nfunc A() int { return 2 +\n",
	})
	head := f.git("rev-parse", "HEAD")

	cases, stats, err := Mine(context.Background(), Repo{Name: "fixture", Commit: head, License: "MIT"}, f.dir, MineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 0 || stats.InvalidReference != 2 || stats.Kept != 0 {
		t.Fatalf("cases %d, stats %+v: want both left out as invalid references", len(cases), stats)
	}

	// Filter applies the same rule to a manifest mined before it.
	g := gitRepo{dir: f.dir}
	var all []Case
	for _, p := range []string{"a.go", "b.go"} {
		c := Case{ID: p, Repo: "fixture", Family: FamilyGo, Path: p}
		for _, side := range []struct {
			rev string
			dst **entry
		}{{"HEAD^1", &c.OursBlob}, {"HEAD^2", &c.TheirsBlob}, {"HEAD", &c.HumanBlob}} {
			e, err := g.entryAt(context.Background(), side.rev, p)
			if err != nil {
				t.Fatal(err)
			}
			*side.dst = e
		}
		all = append(all, c)
	}
	kept, fstats, err := Filter(context.Background(), all, func(string) string { return f.dir })
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 0 || fstats["fixture: "+InvalidMarkers] != 1 || fstats["fixture: "+InvalidSyntax] != 1 {
		t.Fatalf("kept %d, stats %v", len(kept), fstats)
	}
}
