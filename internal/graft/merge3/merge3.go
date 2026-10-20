// Package merge3 is a three-way merge of text by lines. It finds the regions where the two sides
// changed the base, merges the regions where only one side changed it (or both the same way), and
// reports the others as conflicts: as line ranges of each side (the hunks of the conflict package)
// or as a file with git's conflict markers around them.
//
// It is the merge of the conflict package of UserLand: the server computes the hunks with it, and
// the SDK the file with markers that an agent edits, from the sides it reads by object ID.
package merge3

import (
	"bytes"
	"sort"
)

// MaxCells bounds the table of the line diff of one side against the base (the changed middle of
// the files, after the common prefix and suffix): about 2800 changed lines on each side. Merge of
// files whose diff is larger fails.
const MaxCells = 8 << 20

// Range is a half-open range of 0-based lines of one side, [Start, End).
type Range struct {
	Start, End int
}

// Hunk is a conflicting region as a line range of each side. Each range counts the lines of the
// side it names; an empty range is an insertion point before line Start.
type Hunk struct {
	Base, Ours, Theirs Range
}

// region is a stretch of the base that one or both sides changed.
type region struct {
	base, ours, theirs Range
	// changedOurs and changedTheirs say which sides changed it.
	changedOurs, changedTheirs bool
	// conflict is set when both sides changed it, differently.
	conflict bool
}

// Result is the merge of three versions of a text.
type Result struct {
	base, ours, theirs [][]byte
	regions            []region
}

// Merge merges ours and theirs, both changed from base. An absent base (an add/add conflict) is
// empty. It returns false when a diff exceeds MaxCells.
func Merge(base, ours, theirs []byte) (*Result, bool) {
	r := &Result{base: SplitLines(base), ours: SplitLines(ours), theirs: SplitLines(theirs)}
	oursEdits, ok := lineDiff(r.base, r.ours)
	if !ok {
		return nil, false
	}
	theirsEdits, ok := lineDiff(r.base, r.theirs)
	if !ok {
		return nil, false
	}

	type sideEdit struct {
		edit
		theirs bool
	}
	all := make([]sideEdit, 0, len(oursEdits)+len(theirsEdits))
	for _, e := range oursEdits {
		all = append(all, sideEdit{edit: e})
	}
	for _, e := range theirsEdits {
		all = append(all, sideEdit{edit: e, theirs: true})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].start != all[j].start {
			return all[i].start < all[j].start
		}
		return all[i].end < all[j].end
	})

	// delta is, per side, the lines added minus the lines removed by the edits before the region,
	// which maps a base line to the line of that side.
	var oursDelta, theirsDelta int
	for i := 0; i < len(all); {
		start, end := all[i].start, all[i].end
		j := i
		var oursLines, theirsLines, oursCovered, theirsCovered int
		var changedOurs, changedTheirs bool
		// Edits that touch or overlap are one region.
		for ; j < len(all) && (j == i || all[j].start <= end); j++ {
			e := all[j]
			end = max(end, e.end)
			if e.theirs {
				changedTheirs = true
				theirsLines += e.n
				theirsCovered += e.end - e.start
			} else {
				changedOurs = true
				oursLines += e.n
				oursCovered += e.end - e.start
			}
		}
		// A side's text of the region is its edits plus the base lines of the region it kept.
		oursLen := oursLines + (end - start) - oursCovered
		theirsLen := theirsLines + (end - start) - theirsCovered
		reg := region{
			base:        Range{start, end},
			ours:        Range{start + oursDelta, start + oursDelta + oursLen},
			theirs:      Range{start + theirsDelta, start + theirsDelta + theirsLen},
			changedOurs: changedOurs, changedTheirs: changedTheirs,
		}
		reg.conflict = changedOurs && changedTheirs && !sameLines(r.ours[reg.ours.Start:reg.ours.End], r.theirs[reg.theirs.Start:reg.theirs.End])
		r.regions = append(r.regions, reg)
		oursDelta += oursLen - (end - start)
		theirsDelta += theirsLen - (end - start)
		i = j
	}
	return r, true
}

// Hunks returns the conflicting regions, in order.
func (r *Result) Hunks() []Hunk {
	var hunks []Hunk
	for _, reg := range r.regions {
		if reg.conflict {
			hunks = append(hunks, Hunk{Base: reg.base, Ours: reg.ours, Theirs: reg.theirs})
		}
	}
	return hunks
}

// Labels name the sides in the conflict markers.
type Labels struct {
	Ours, Base, Theirs string
}

// WithMarkers returns the merged text: the regions that only one side changed (or both the same
// way) take that change, and every conflicting region is written between git's diff3 conflict
// markers, "<<<<<<< ours", "||||||| base", "=======" and ">>>>>>> theirs", followed by the labels.
// A side of a conflict that does not end with a newline gets one before the next marker. The
// markers and that newline end with CRLF when git's would (crlfNeeded), LF otherwise.
func (r *Result) WithMarkers(l Labels) []byte {
	var out bytes.Buffer
	pos := 0
	eol := "\n"
	write := func(lines [][]byte) {
		for _, line := range lines {
			out.Write(line)
		}
	}
	writeSide := func(lines [][]byte) {
		write(lines)
		if out.Len() > 0 && out.Bytes()[out.Len()-1] != '\n' {
			out.WriteString(eol)
		}
	}
	marker := func(m, label string) {
		out.WriteString(m)
		if label != "" {
			out.WriteString(" " + label)
		}
		out.WriteString(eol)
	}
	for _, reg := range r.regions {
		write(r.base[pos:reg.base.Start])
		pos = reg.base.End
		switch {
		case reg.conflict:
			eol = "\n"
			if r.crlfNeeded(reg) {
				eol = "\r\n"
			}
			marker("<<<<<<<", l.Ours)
			writeSide(r.ours[reg.ours.Start:reg.ours.End])
			marker("|||||||", l.Base)
			writeSide(r.base[reg.base.Start:reg.base.End])
			marker("=======", "")
			writeSide(r.theirs[reg.theirs.Start:reg.theirs.End])
			marker(">>>>>>>", l.Theirs)
		case reg.changedOurs:
			write(r.ours[reg.ours.Start:reg.ours.End])
		default:
			write(r.theirs[reg.theirs.Start:reg.theirs.End])
		}
	}
	write(r.base[pos:])
	return out.Bytes()
}

// crlfNeeded reports whether the markers of the conflicting region reg end with CRLF, as git's
// do: the line of ours before the conflict (its first line when the conflict starts the file), the
// same line of theirs, and the first line of the base must all end with CRLF. A side whose line
// ending cannot be told (an empty side, or a single line without one) does not decide, but when
// the base cannot tell, the markers end with LF.
func (r *Result) crlfNeeded(reg region) bool {
	before := func(start int) int { return max(start-1, 0) }
	if lineEndingOf(r.ours, before(reg.ours.Start)) == endingLF {
		return false
	}
	if lineEndingOf(r.theirs, before(reg.theirs.Start)) == endingLF {
		return false
	}
	return lineEndingOf(r.base, 0) == endingCRLF
}

// lineEnding is the line ending of a line, for crlfNeeded.
type lineEnding int

const (
	endingUnknown lineEnding = iota
	endingLF
	endingCRLF
)

// lineEndingOf returns the line ending of line i of lines. The last line of a file may have none:
// its ending is then the one of the line before it, and unknown when there is none.
func lineEndingOf(lines [][]byte, i int) lineEnding {
	ending := func(line []byte) lineEnding {
		if bytes.HasSuffix(line, []byte("\r\n")) {
			return endingCRLF
		}
		return endingLF
	}
	switch {
	case len(lines) == 0:
		return endingUnknown
	case i < len(lines)-1 || bytes.HasSuffix(lines[i], []byte("\n")):
		return ending(lines[i])
	case i == 0:
		return endingUnknown
	default:
		return ending(lines[i-1])
	}
}

func sameLines(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// SplitLines splits b into lines that keep their terminator.
func SplitLines(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}
	parts := bytes.SplitAfter(b, []byte("\n"))
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// Cost returns the cells of the two line-diff tables that Merge of base, ours and theirs fills, so
// that a caller can bound the work of many merges, and whether both fit MaxCells: when one does
// not, Merge fails at no cost, and cells is 0. Cost splits the lines and compares the common prefix
// and suffix, which takes time linear in the size of the files.
func Cost(base, ours, theirs []byte) (cells int, fits bool) {
	b := SplitLines(base)
	for _, side := range [][]byte{ours, theirs} {
		s := SplitLines(side)
		pre, suf := trim(b, s)
		n, m := len(b)-pre-suf, len(s)-pre-suf
		if n == 0 && m == 0 {
			continue
		}
		if (n+1)*(m+1) > MaxCells {
			return 0, false
		}
		cells += (n + 1) * (m + 1)
	}
	return cells, true
}

// trim returns the number of lines that a and b share at their start and, after those, at their
// end.
func trim(a, b [][]byte) (pre, suf int) {
	for pre < len(a) && pre < len(b) && bytes.Equal(a[pre], b[pre]) {
		pre++
	}
	for suf < len(a)-pre && suf < len(b)-pre && bytes.Equal(a[len(a)-1-suf], b[len(b)-1-suf]) {
		suf++
	}
	return pre, suf
}

// edit replaces the base lines [start, end) with n lines of a side.
type edit struct {
	start, end int
	n          int
}

// lineDiff returns the line edits that turn a into b, from a longest common subsequence of the
// lines after the common prefix and suffix. It returns false when the table would exceed MaxCells.
func lineDiff(a, b [][]byte) ([]edit, bool) {
	pre, suf := trim(a, b)
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	n, m := len(am), len(bm)
	if n == 0 && m == 0 {
		return nil, true
	}
	w := m + 1
	if (n+1)*w > MaxCells {
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

	var edits []edit
	cur := -1
	for i, j := 0, 0; i < n || j < m; {
		if i < n && j < m && bytes.Equal(am[i], bm[j]) {
			cur = -1
			i++
			j++
			continue
		}
		if cur < 0 {
			edits = append(edits, edit{start: pre + i, end: pre + i})
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
