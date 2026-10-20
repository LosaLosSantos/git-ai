package resolve

import (
	"context"
	"slices"
	"strings"
)

// CompositionVerdict checks a whole resolved file against the merge of base, ours and theirs, as the
// measurement did (PLAN10 15.6): "pass" (nothing dropped, nothing repeated), "drops", "duplicates", or
// "outside" (the result does not keep the clean segments of the merge in order: it changed lines
// outside the conflicts). ok is false when the merge has no conflicting region to check. The problems
// are the exact words of the report. The regions are those of git's merge (llmFileRegions), as the
// prototype's were, and the bench compares the verdicts with the prototype's.
func CompositionVerdict(base, ours, theirs, result []byte) (verdict string, problems []string, ok bool) {
	regions, err := llmFileRegions(context.Background(), base, ours, theirs, true)
	if err != nil {
		return "", nil, false
	}
	var segs [][]string
	var conflicts []*llmRegion
	var seg []string
	for i := range regions {
		r := &regions[i]
		if r.clean {
			seg = append(seg, strings.Split(strings.TrimSuffix(r.merged, "\n"), "\n")...)
			if r.merged == "" {
				seg = seg[:len(seg)-1]
			}
			continue
		}
		segs = append(segs, seg)
		seg = nil
		conflicts = append(conflicts, r)
	}
	segs = append(segs, seg)
	if len(conflicts) == 0 {
		return "", nil, false
	}
	for i := range segs {
		for j := range segs[i] {
			segs[i][j] = strings.TrimRight(segs[i][j], " \t\r\f\v")
		}
	}
	// Like the lines of the result, the merge drops the blank lines at the end of the file.
	for last := len(segs) - 1; len(segs[last]) > 0 && segs[last][len(segs[last])-1] == ""; {
		segs[last] = segs[last][:len(segs[last])-1]
	}
	groups := llmSlots(llmNormLines(string(result)), segs)
	if groups == nil {
		return "outside", []string{"changes lines outside the conflicts"}, true
	}
	var all []string
	for _, g := range groups {
		slot := llmCount(llmNonBlank(g.slot))
		for _, i := range g.regions {
			r := conflicts[i]
			c := llmComposeLines(llmNormLines(r.base), llmNormLines(r.ours), llmNormLines(r.theirs), r.start, slot)
			all = append(all, c.problems...)
		}
	}
	verdict = "pass"
	for _, p := range all {
		if strings.HasPrefix(p, "drops") {
			verdict = "drops"
			break
		}
		verdict = "duplicates"
	}
	return verdict, all, true
}

// llmSlotGroup is the stretch of the result between two clean segments, with the regions it answers:
// two regions with no clean line between them share one.
type llmSlotGroup struct {
	regions []int
	slot    []string
}

// llmSlots finds the slots of lines between the clean segments of the merge, or nil when lines do
// not keep them in order.
func llmSlots(lines []string, segs [][]string) []llmSlotGroup {
	if len(segs[0]) > len(lines) || !slices.Equal(segs[0], lines[:len(segs[0])]) {
		return nil
	}
	pos := len(segs[0])
	last := segs[len(segs)-1]
	end := len(lines) - len(last)
	if end < pos || !slices.Equal(lines[end:], last) {
		return nil
	}
	var groups []llmSlotGroup
	cur := []int{0}
	for i := 1; i < len(segs)-1; i++ {
		seg := segs[i]
		if len(seg) == 0 {
			cur = append(cur, i)
			continue
		}
		found := -1
		for j := pos; j <= end-len(seg); j++ {
			if slices.Equal(lines[j:j+len(seg)], seg) {
				found = j
				break
			}
		}
		if found < 0 {
			return nil
		}
		groups = append(groups, llmSlotGroup{regions: cur, slot: lines[pos:found]})
		cur, pos = []int{i}, found+len(seg)
	}
	return append(groups, llmSlotGroup{regions: cur, slot: lines[pos:end]})
}
