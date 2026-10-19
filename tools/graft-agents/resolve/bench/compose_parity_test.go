package bench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
)

// TestCompositionParity compares resolve.CompositionVerdict (the port of the composition check,
// PLAN10 15.3) with the verdicts of the prototype it was frozen from (compose2.py, which takes the
// regions of git merge-file) on the accepted answers of a stored run. It is an offline measurement
// and costs nothing, but needs the mined clones, so it runs only when GRAFT_COMPOSE_PARITY names a
// directory with report.json (a run's report), answers/ (its answers), manifest.json and
// python.json (the prototype's verdicts by case ID), and GRAFT_COMPOSE_CLONES the directory of the
// clones (<owner>__<repo>.git). The port takes the regions of the llm strategy's own line diff, so a
// few verdicts may differ; the test fails only if the share that differs is over 5%, and logs every
// difference.
func TestCompositionParity(t *testing.T) {
	dir := os.Getenv("GRAFT_COMPOSE_PARITY")
	clones := os.Getenv("GRAFT_COMPOSE_CLONES")
	if dir == "" || clones == "" {
		t.Skip("set GRAFT_COMPOSE_PARITY and GRAFT_COMPOSE_CLONES to run the parity measurement")
	}
	var report struct {
		Results []Result `json:"results"`
	}
	read := func(name string, v any) {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatal(err)
		}
	}
	read("report.json", &report)
	var python map[string]struct {
		Verdict string `json:"verdict"`
	}
	read("python.json", &python)
	var manifest struct {
		Cases []Case `json:"cases"`
	}
	read("manifest.json", &manifest)
	cases := map[string]Case{}
	for _, c := range manifest.Cases {
		cases[c.ID] = c
	}

	ctx := context.Background()
	same, differ, skipped := 0, 0, 0
	matrix := map[string]int{}
	var lines []string
	for _, r := range report.Results {
		want, ok := python[r.ID]
		if !ok || want.Verdict == "no-regions" {
			continue
		}
		c := cases[r.ID]
		g := gitRepo{dir: filepath.Join(clones, strings.ReplaceAll(c.Repo, "/", "__")+".git")}
		var sides [3][]byte
		for i, e := range []*entry{c.BaseBlob, c.OursBlob, c.TheirsBlob} {
			if e == nil {
				continue
			}
			b, err := g.blob(ctx, e.OID)
			if err != nil {
				t.Fatal(err)
			}
			sides[i] = b
		}
		answer, err := os.ReadFile(answerPath(filepath.Join(dir, "answers"), r.ID))
		if err != nil {
			t.Fatal(err)
		}
		got, problems, ok := resolve.CompositionVerdict(sides[0], sides[1], sides[2], answer)
		if !ok {
			skipped++
			continue
		}
		matrix[want.Verdict+" -> "+got]++
		if got == want.Verdict {
			same++
			continue
		}
		differ++
		lines = append(lines, r.ID+" "+r.Path+": prototype "+want.Verdict+", port "+got+" "+strings.Join(problems, " | "))
	}
	keys := make([]string, 0, len(matrix))
	for k := range matrix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-28s %d", k, matrix[k])
	}
	for _, l := range lines {
		t.Log(l)
	}
	t.Logf("same %d, differ %d, port found no region %d", same, differ, skipped)
	if total := same + differ; total > 0 && differ*20 > total {
		t.Fatalf("%d of %d verdicts differ from the prototype's", differ, total)
	}
}
