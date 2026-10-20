package resolve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The regions of a file are the regions of git's own merge of the three versions (PLAN10 15.10): what
// git calls a conflict is what the model is asked, and a file in which git sees none is merged
// without the model, so "no conflict" means what it means to the server that listed the file. Our own
// line diff (llmRegions) disagreed with git in 13 of the 300 cases of the benchmark, 7 of them
// answered wrong, and stays only as the fallback for a graft-resolve that has no git to run.
//
// git merge-file --diff3 is used (the style of the measurement of 15.6, which shows the base of each
// conflict). The merge is made on three files in a private temporary directory, with markers whose
// labels carry a random token and whose size is longer than any run of marker characters that starts
// a line of the file, so that no line of the file can be taken for a marker.

// llmFileRegions splits the file into the regions of the strategy: clean ones with their merged text,
// and the conflicting ones with their three texts, in the order of the file, so that the result of a
// resolution is the concatenation of the regions (llmJoin). useGit says whether to ask git; without it,
// or when git cannot be used for the file, the regions are those of our own line diff.
func llmFileRegions(ctx context.Context, base, ours, theirs []byte, useGit bool) ([]llmRegion, error) {
	if useGit {
		if regions, ok := llmGitRegions(ctx, base, ours, theirs); ok {
			return regions, nil
		}
	}
	b := llmSplitLines(base)
	oursEdits, err := llmDiff(b, llmSplitLines(ours), defaultLLMMaxDiffCells)
	if err != nil {
		return nil, err
	}
	theirsEdits, err := llmDiff(b, llmSplitLines(theirs), defaultLLMMaxDiffCells)
	if err != nil {
		return nil, err
	}
	return llmUnified(b, llmRegions(b, oursEdits, theirsEdits)), nil
}

// llmUnified turns the regions of our line diff, which are slices of the base between gaps, into the
// regions of llmFileRegions: the gaps are clean regions of their own.
func llmUnified(base []string, regions []llmRegion) []llmRegion {
	var out []llmRegion
	pos := 0
	for _, r := range regions {
		if pos < r.start {
			out = append(out, llmRegion{clean: true, merged: strings.Join(base[pos:r.start], "")})
		}
		if r.clean {
			out = append(out, llmRegion{clean: true, merged: r.merged})
		} else {
			out = append(out, r)
		}
		pos = r.end
	}
	if pos < len(base) {
		out = append(out, llmRegion{clean: true, merged: strings.Join(base[pos:], "")})
	}
	return out
}

// llmGitUsable reports whether git merge-file can split the file: the three sides are present, end
// with a newline (a conflict marker line always begins a line, which changes the text of a region at
// the end of a file without one), and the git binary is there.
func llmGitUsable(base, ours, theirs []byte) (string, bool) {
	for _, side := range [][]byte{base, ours, theirs} {
		if side == nil || (len(side) > 0 && side[len(side)-1] != '\n') {
			return "", false
		}
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return "", false
	}
	return git, true
}

// llmMarkerSize returns a marker size longer than any run of one marker character at the start of a
// line of the sides, and at least git's own 7.
func llmMarkerSize(sides ...[]byte) int {
	size := 7
	for _, side := range sides {
		for _, line := range bytes.Split(side, []byte("\n")) {
			if len(line) == 0 || !bytes.ContainsRune([]byte("<|=>"), rune(line[0])) {
				continue
			}
			run := 1
			for run < len(line) && line[run] == line[0] {
				run++
			}
			size = max(size, run+1)
		}
	}
	return size
}

// llmGitRegions asks git merge-file for the regions of the file. It reports false when git cannot be
// used or fails, and the caller falls back to the regions of our own diff.
func llmGitRegions(ctx context.Context, base, ours, theirs []byte) ([]llmRegion, bool) {
	git, ok := llmGitUsable(base, ours, theirs)
	if !ok {
		return nil, false
	}
	dir, err := os.MkdirTemp("", "graft-merge-")
	if err != nil {
		return nil, false
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for name, content := range map[string][]byte{"ours": ours, "base": base, "theirs": theirs} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			return nil, false
		}
	}
	var token [6]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, false
	}
	tag := hex.EncodeToString(token[:])
	size := llmMarkerSize(base, ours, theirs)
	labels := [3]string{"ours-" + tag, "base-" + tag, "theirs-" + tag}
	cmd := exec.CommandContext(ctx, git, "merge-file", "-p", "--diff3", fmt.Sprintf("--marker-size=%d", size),
		"-L", labels[0], "-L", labels[1], "-L", labels[2], "ours", "base", "theirs")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	out, err := cmd.Output()
	conflicts := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() < 1 || exit.ExitCode() > 127 {
			return nil, false
		}
		conflicts = exit.ExitCode()
	}
	regions, parsed, ok := llmParseDiff3(string(out), size, labels)
	if !ok || (conflicts <= 127 && parsed != conflicts) {
		return nil, false
	}
	llmLocateBase(llmSplitLines(base), regions)
	return regions, true
}

// llmParseDiff3 reads the output of git merge-file --diff3 with the given marker size and labels into
// regions. It reports the number of conflicts, and false if the output is not well formed.
func llmParseDiff3(out string, size int, labels [3]string) (regions []llmRegion, conflicts int, ok bool) {
	open := strings.Repeat("<", size) + " " + labels[0]
	middle := strings.Repeat("|", size) + " " + labels[1]
	split := strings.Repeat("=", size)
	closing := strings.Repeat(">", size) + " " + labels[2]

	var clean strings.Builder
	flush := func() {
		if clean.Len() > 0 {
			regions = append(regions, llmRegion{clean: true, merged: clean.String()})
			clean.Reset()
		}
	}
	var current *[3]strings.Builder // ours, base, theirs
	section := 0
	for _, line := range llmSplitLines([]byte(out)) {
		trimmed := strings.TrimSuffix(line, "\n")
		switch {
		case current == nil && trimmed == open:
			flush()
			current, section = new([3]strings.Builder), 0
		case current != nil && section == 0 && trimmed == middle:
			section = 1
		case current != nil && section <= 1 && trimmed == split:
			section = 2
		case current != nil && section == 2 && trimmed == closing:
			regions = append(regions, llmRegion{
				ours: current[0].String(), base: current[1].String(), theirs: current[2].String(),
			})
			conflicts++
			current = nil
		case current != nil:
			current[section].WriteString(line)
		default:
			clean.WriteString(line)
		}
	}
	if current != nil {
		return nil, 0, false
	}
	flush()
	return regions, conflicts, true
}

// llmLocateBase sets, for each conflicting region, the line of the base where its base text starts
// (the base text of a conflict is a slice of the base, and the conflicts come in the order of the
// base). The report words the lines of the base with it. A conflict whose base text is empty (both
// sides inserted) starts where the previous one ended; a text found nowhere (it cannot happen) starts
// there as well.
func llmLocateBase(base []string, regions []llmRegion) {
	pos := 0
	for i := range regions {
		r := &regions[i]
		if r.clean {
			continue
		}
		lines := llmSplitLines([]byte(r.base))
		start := pos
		for j := pos; len(lines) > 0 && j+len(lines) <= len(base); j++ {
			if llmEqualLines(base[j:j+len(lines)], lines) {
				start = j
				break
			}
		}
		r.start, r.end = start, start+len(lines)
		pos = r.end
	}
}

func llmEqualLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// llmJoin is the file the regions make: the merged text of the clean ones and the answer of the
// conflicting ones.
func llmJoin(regions []llmRegion) string {
	s, _ := llmJoinSpans(regions)
	return s
}

// llmJoinSpans is llmJoin, with the lines of each conflicting region's answer in the result:
// spans[i] is [first, end) in 0-based lines of the file.
func llmJoinSpans(regions []llmRegion) (string, map[int][2]int) {
	var sb strings.Builder
	spans := map[int][2]int{}
	lines := 0
	count := func(s string) int {
		n := strings.Count(s, "\n")
		if s != "" && !strings.HasSuffix(s, "\n") {
			n++
		}
		return n
	}
	for i := range regions {
		r := &regions[i]
		text := r.merged
		if !r.clean {
			text = r.answer
		}
		first := lines
		sb.WriteString(text)
		lines += count(text)
		if !r.clean {
			spans[i] = [2]int{first, lines}
		}
	}
	return sb.String(), spans
}

// LLMConflictRegionsBy is LLMConflictRegions with the choice of the splitter: git merge-file, or our own
// line diff. The bench compares the two. It returns -1 when a diff is too large to compute.
func LLMConflictRegionsBy(base, ours, theirs []byte, useGit bool) int {
	regions, err := llmFileRegions(context.Background(), base, ours, theirs, useGit)
	if err != nil {
		return -1
	}
	n := 0
	for _, r := range regions {
		if !r.clean {
			n++
		}
	}
	return n
}
