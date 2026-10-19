package resolve

import (
	"fmt"
	"sort"
	"strings"
)

// The composition check of a region's answer (PLAN10 15.3, 15.6): a conflict is two changes to one
// base, and the answer for a region should keep what each side changed unless the other side
// changed the same base lines. The definition is the one frozen on 2026-10-19 before it was measured
// (compose2.py of the measurement, sha256 9396326b...); this file is its port to the regions of the
// llm strategy, and is checked against it (llm_compose_test.go):
//
//   - A region's answer is a slot of lines. Each side's change is the list of hunks of the line diff
//     base -> side inside the region: a base range [a, b) replaced by the side's lines (an insertion
//     has a == b).
//   - A hunk is contested when a hunk of the other side overlaps its base lines (an insertion: when
//     the other hunk's range holds its point, ends included). Both sides edited the same lines, so the
//     answer may compose them freely (one wins, or a third version): nothing line-level to verify,
//     never a drop.
//   - An uncontested hunk must be in the slot. It drops the change when a non-blank line it adds is
//     missing from the slot, or when a non-blank base line it deletes is in the slot more times than
//     the side's region and the other side's additions account for.
//   - A non-blank line both sides added, in the slot more times than either side has it, is a
//     duplicate.
//   - A non-blank slot line in none of base, ours and theirs is reported (a human writes new code:
//     that is no composition error).
//
// Lines are compared after the judge's normalization (CRLF, trailing whitespace) and stripped. It is
// a report, never a gate: the measurement showed that the check does not move precision (15.6). What
// it does is name, in exact words, what the answer dropped or repeated, which a repair round (15.10)
// can quote back to the model, and which goes to the agent with the proposal.

// llmComposition is the check of one region's answer.
type llmComposition struct {
	// problems are the drops and the duplicates, in the exact words that the report and a repair round
	// use.
	problems []string
	// kept is the number of uncontested hunks the slot keeps, contested the number of contested ones.
	kept, contested int
	// neither says that the slot holds a line found in none of the three versions.
	neither bool
}

// llmNormLines returns the lines of text as the judge compares them: CRLF as LF, trailing whitespace
// cut, the blank lines at the end dropped.
func llmNormLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\r\f\v")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// llmNonBlank returns the stripped, non-blank lines of lines.
func llmNonBlank(lines []string) []string {
	var out []string
	for _, l := range lines {
		if s := strings.TrimSpace(l); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func llmCount(lines []string) map[string]int {
	c := make(map[string]int, len(lines))
	for _, l := range lines {
		c[l]++
	}
	return c
}

// llmSubtract is the multiset difference a - b (counts floored at zero).
func llmSubtract(a, b map[string]int) map[string]int {
	out := map[string]int{}
	for k, n := range a {
		if d := n - b[k]; d > 0 {
			out[k] = d
		}
	}
	return out
}

// llmHunks are the hunks of the line diff from base to side, on the stripped lines.
func llmHunks(base, side []string) []llmEdit {
	strip := func(in []string) []string {
		out := make([]string, len(in))
		for i, l := range in {
			out[i] = strings.TrimSpace(l)
		}
		return out
	}
	edits, err := llmDiff(strip(base), strip(side), defaultLLMMaxDiffCells)
	if err != nil {
		return nil
	}
	return edits
}

// llmHunksOverlap reports whether two hunks of different sides contest each other.
func llmHunksOverlap(h, k llmEdit) bool {
	switch {
	case h.start == h.end: // an insertion: contested when the other hunk's range holds its point
		return k.start <= h.start && h.start <= k.end
	case k.start == k.end:
		return h.start <= k.start && k.start <= h.end
	}
	return h.start < k.end && k.start < h.end
}

// llmClipLine cuts a line quoted in a report to a readable length.
func llmClipLine(s string) string {
	const n = 100
	if len(s) <= n {
		return s
	}
	return llmClip(s, n) + "..."
}

// llmBaseRange words the base lines of a hunk, first being the 0-based first line of the region in
// the file's base.
func llmBaseRange(first int, h llmEdit) string {
	if h.start == h.end {
		return fmt.Sprintf("after base line %d", first+h.start)
	}
	if h.end-h.start == 1 {
		return fmt.Sprintf("at base line %d", first+h.start+1)
	}
	return fmt.Sprintf("at base lines %d-%d", first+h.start+1, first+h.end)
}

// llmCompose checks the answer of a conflicting region of the file whose base starts at line first
// (0-based) of the region.
func llmCompose(r *llmRegion, first int, answer string) llmComposition {
	return llmComposeLines(llmNormLines(r.base), llmNormLines(r.ours), llmNormLines(r.theirs), first, llmCount(llmNonBlank(llmNormLines(answer))))
}

// llmComposeLines is the check of a region given as normalized lines, against the non-blank lines
// of the slot of the result, counted.
func llmComposeLines(b, o, t []string, first int, slot map[string]int) llmComposition {
	var c llmComposition
	B, O, T := llmCount(llmNonBlank(b)), llmCount(llmNonBlank(o)), llmCount(llmNonBlank(t))
	sides := []struct {
		name        string
		mine, other []llmEdit
		mineLines   map[string]int
		otherAdded  map[string]int
	}{
		{"OURS", llmHunks(b, o), llmHunks(b, t), O, llmSubtract(T, B)},
		{"THEIRS", llmHunks(b, t), llmHunks(b, o), T, llmSubtract(O, B)},
	}
	for _, s := range sides {
		for _, h := range s.mine {
			contested := false
			for _, k := range s.other {
				if llmHunksOverlap(h, k) {
					contested = true
					break
				}
			}
			if contested {
				c.contested++
				continue
			}
			dropped := false
			for _, line := range llmNonBlank(h.repl) {
				if slot[line] == 0 {
					c.problems = append(c.problems, fmt.Sprintf("drops %s' change %s: the line %q it adds is missing", s.name, llmBaseRange(first, h), llmClipLine(line)))
					dropped = true
					break
				}
			}
			if !dropped {
				for _, line := range llmNonBlank(b[h.start:h.end]) {
					if slot[line] > s.mineLines[line]+s.otherAdded[line] {
						c.problems = append(c.problems, fmt.Sprintf("drops %s' deletion %s: the line %q it deletes is still there", s.name, llmBaseRange(first, h), llmClipLine(line)))
						dropped = true
						break
					}
				}
			}
			if !dropped {
				c.kept++
			}
		}
	}
	// A line both sides added and the answer repeats. The map order is not deterministic: sort.
	added := llmSubtract(O, B)
	var dup []string
	for line, n := range llmSubtract(T, B) {
		if added[line] > 0 && n > 0 && slot[line] > max(O[line], T[line]) {
			dup = append(dup, line)
		}
	}
	sort.Strings(dup)
	for _, line := range dup {
		c.problems = append(c.problems, fmt.Sprintf("duplicates a line both sides added: %q", llmClipLine(line)))
	}
	for line := range slot {
		if B[line] == 0 && O[line] == 0 && T[line] == 0 {
			c.neither = true
			break
		}
	}
	return c
}
