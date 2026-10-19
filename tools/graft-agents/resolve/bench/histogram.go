package bench

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
)

// PatchDiffStats counts, for one language, how often the PATCH the llm strategy sends (its own LCS
// line diff) differs from git diff --histogram on the same texts: the diff synthmerge uses.
type PatchDiffStats struct {
	Cases           int `json:"cases"`
	Regions         int `json:"regions"`
	DifferentRegion int `json:"different_regions"`
	DifferentCases  int `json:"different_cases"`
}

// PatchDiffs compares, for the content conflicts of cases, the PATCH of every conflicting region
// with the output of git diff --no-index --histogram on the same two texts, with contextLines lines
// of context. It makes no model call.
func PatchDiffs(ctx context.Context, cases []Case, clone func(repo string) string, contextLines int) (map[string]PatchDiffStats, error) {
	stats := map[string]PatchDiffStats{}
	work, err := os.MkdirTemp("", "graft-bench-histogram-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	from, to := filepath.Join(work, "from"), filepath.Join(work, "to")

	for _, c := range cases {
		if c.Kind != "content" || c.OursBlob == nil || c.TheirsBlob == nil {
			continue
		}
		g := gitRepo{dir: clone(c.Repo)}
		var sides [3][]byte
		for i, e := range []*entry{c.BaseBlob, c.OursBlob, c.TheirsBlob} {
			if e == nil {
				continue
			}
			if sides[i], err = g.blob(ctx, e.OID); err != nil {
				return nil, err
			}
		}
		patches := resolve.LLMPatches(sides[0], sides[1], sides[2], contextLines)
		lang := LanguageOf(c.Path)
		s := stats[lang]
		s.Cases++
		different := false
		for _, p := range patches {
			if err := os.WriteFile(from, []byte(p.From), 0o600); err != nil {
				return nil, err
			}
			if err := os.WriteFile(to, []byte(p.To), 0o600); err != nil {
				return nil, err
			}
			out, err := exec.CommandContext(ctx, "git", "diff", "--no-index", "--no-color", "--histogram",
				fmt.Sprintf("-U%d", contextLines), from, to).Output()
			// git diff --no-index exits 1 when the files differ.
			if exit, ok := err.(*exec.ExitError); err != nil && (!ok || exit.ExitCode() != 1) {
				return nil, fmt.Errorf("git diff for %s: %w", c.ID, err)
			}
			s.Regions++
			if gitHunks(string(out)) != p.Patch {
				s.DifferentRegion++
				different = true
			}
		}
		if different {
			s.DifferentCases++
		}
		stats[lang] = s
	}
	return stats, nil
}

// gitHunks returns the hunks of a git diff, without its header lines.
func gitHunks(diff string) string {
	if i := strings.Index(diff, "\n@@ "); i >= 0 {
		return diff[i+1:]
	}
	if strings.HasPrefix(diff, "@@ ") {
		return diff
	}
	return ""
}
