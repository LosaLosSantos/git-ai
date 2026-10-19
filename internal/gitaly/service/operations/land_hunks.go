package operations

import (
	"bytes"
	"sort"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// landHunkMaxCells bounds the table of the line diff of one side against the base (the changed
// middle of the files, after the common prefix and suffix): about 2800 changed lines on each side.
// A file whose diff is larger gets no hunks in the conflict package; the agent still has its sides.
const landHunkMaxCells = 8 << 20

// landLineEdit replaces the base lines [start, end) with n lines of a side.
type landLineEdit struct {
	start, end int
	n          int
}

// landConflictHunks returns the conflicting regions of a three-way merge of base, ours and theirs,
// as line ranges of each side (LandConflictHunk). A region is the union of the base lines that one
// or both sides changed, where edits that touch or overlap are one region; it conflicts when both
// sides changed it, differently. Each range counts the lines of its own side, 1-based and
// half-open. It returns nil when a diff exceeds landHunkMaxCells.
func landConflictHunks(base, ours, theirs []byte) []*gitalypb.LandConflictHunk {
	b, o, t := landSplitLines(base), landSplitLines(ours), landSplitLines(theirs)
	oursEdits, ok := landLineDiff(b, o)
	if !ok {
		return nil
	}
	theirsEdits, ok := landLineDiff(b, t)
	if !ok {
		return nil
	}

	type sideEdit struct {
		landLineEdit
		theirs bool
	}
	all := make([]sideEdit, 0, len(oursEdits)+len(theirsEdits))
	for _, e := range oursEdits {
		all = append(all, sideEdit{landLineEdit: e})
	}
	for _, e := range theirsEdits {
		all = append(all, sideEdit{landLineEdit: e, theirs: true})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].start != all[j].start {
			return all[i].start < all[j].start
		}
		return all[i].end < all[j].end
	})

	var hunks []*gitalypb.LandConflictHunk
	// delta is, per side, the lines added minus the lines removed by the edits before the region,
	// which maps a base line to the line of that side.
	var oursDelta, theirsDelta int
	for i := 0; i < len(all); {
		start, end := all[i].start, all[i].end
		j := i
		var oursLines, theirsLines int
		var oursChanged, theirsChanged bool
		var oursCovered, theirsCovered int // base lines replaced by each side's edits in the region
		for ; j < len(all) && (j == i || all[j].start <= end); j++ {
			e := all[j]
			end = max(end, e.end)
			if e.theirs {
				theirsChanged = true
				theirsLines += e.n
				theirsCovered += e.end - e.start
			} else {
				oursChanged = true
				oursLines += e.n
				oursCovered += e.end - e.start
			}
		}
		// A side's text of the region is its edits plus the base lines of the region it kept.
		oursLen := oursLines + (end - start) - oursCovered
		theirsLen := theirsLines + (end - start) - theirsCovered

		if oursChanged && theirsChanged && !landSameText(b, o, t, start, end, oursDelta, theirsDelta, oursLen, theirsLen) {
			hunks = append(hunks, &gitalypb.LandConflictHunk{
				Base:   landRange(start, end),
				Ours:   landRange(start+oursDelta, start+oursDelta+oursLen),
				Theirs: landRange(start+theirsDelta, start+theirsDelta+theirsLen),
			})
		}
		oursDelta += oursLen - (end - start)
		theirsDelta += theirsLen - (end - start)
		i = j
	}
	return hunks
}

// landSameText reports whether both sides wrote the same lines into the region, which is a clean
// region and not a conflict.
func landSameText(b, o, t [][]byte, start, end, oursDelta, theirsDelta, oursLen, theirsLen int) bool {
	if oursLen != theirsLen {
		return false
	}
	for k := 0; k < oursLen; k++ {
		if !bytes.Equal(o[start+oursDelta+k], t[start+theirsDelta+k]) {
			return false
		}
	}
	return true
}

// landRange returns the 1-based half-open range of the 0-based lines [start, end).
func landRange(start, end int) *gitalypb.LineRange {
	return &gitalypb.LineRange{Start: uint32(start + 1), End: uint32(end + 1)}
}

// landSplitLines splits b into lines that keep their terminator.
func landSplitLines(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}
	parts := bytes.SplitAfter(b, []byte("\n"))
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// landLineDiff returns the line edits that turn a into b, from a longest common subsequence of the
// lines after the common prefix and suffix. It returns false when the table would exceed
// landHunkMaxCells.
func landLineDiff(a, b [][]byte) ([]landLineEdit, bool) {
	pre := 0
	for pre < len(a) && pre < len(b) && bytes.Equal(a[pre], b[pre]) {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && bytes.Equal(a[len(a)-1-suf], b[len(b)-1-suf]) {
		suf++
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	n, m := len(am), len(bm)
	if n == 0 && m == 0 {
		return nil, true
	}
	w := m + 1
	if (n+1)*w > landHunkMaxCells {
		return nil, false
	}

	// lcs[i*w+j] is the LCS length of am[i:] and bm[j:].
	lcs := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case bytes.Equal(am[i], bm[j]):
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
				lcs[i*w+j] = lcs[(i+1)*w+j]
			default:
				lcs[i*w+j] = lcs[i*w+j+1]
			}
		}
	}

	var edits []landLineEdit
	cur := -1
	for i, j := 0, 0; i < n || j < m; {
		if i < n && j < m && bytes.Equal(am[i], bm[j]) {
			cur = -1
			i++
			j++
			continue
		}
		if cur < 0 {
			edits = append(edits, landLineEdit{start: pre + i, end: pre + i})
			cur = len(edits) - 1
		}
		if i < n && (j == m || lcs[(i+1)*w+j] >= lcs[i*w+j+1]) {
			i++
			edits[cur].end = pre + i
		} else {
			edits[cur].n++
			j++
		}
	}
	return edits, true
}
