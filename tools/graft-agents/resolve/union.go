package resolve

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/feder-cr/open_scion/tools/graft-agents/protocol"
)

// maxUnionCells bounds the LCS table (lines of the middle part of both
// sides, multiplied) so a pathological file cannot exhaust memory.
const maxUnionCells = 4_000_000

// union is a line-based three-way merge in which conflicting regions keep
// both sides: ours first, then theirs. It is meant for append-style files
// such as CHANGELOG.md. Changes that do not overlap are merged normally.
type union struct{}

func newUnion(spec StrategySpec) (Strategy, error) {
	if err := allowOnly(spec, "union", "paths"); err != nil {
		return nil, err
	}
	if len(spec.Paths) == 0 {
		return nil, fmt.Errorf("strategy %q: union needs paths", spec.Name)
	}
	return union{}, nil
}

func (union) Resolve(_ context.Context, f File, _ protocol.Request) (Resolution, bool, error) {
	if !regularFile(f.Ours) || !regularFile(f.Theirs) || f.Ours.Mode != f.Theirs.Mode {
		return Resolution{}, false, nil
	}
	if f.Base != nil && isBinary(f.Base.Content) {
		return Resolution{}, false, nil
	}
	if isBinary(f.Ours.Content) || isBinary(f.Theirs.Content) {
		return Resolution{}, false, nil
	}

	var base []byte
	if f.Base != nil {
		base = f.Base.Content
	}
	merged, err := unionMerge(base, f.Ours.Content, f.Theirs.Content)
	if err != nil {
		return Resolution{}, false, err
	}
	return Resolution{Action: protocol.ActionWrite, Mode: f.Ours.Mode, Content: merged}, true, nil
}

// unionMerge merges ours and theirs against base line by line.
func unionMerge(base, ours, theirs []byte) ([]byte, error) {
	b := splitLines(base)
	o := splitLines(ours)
	t := splitLines(theirs)

	eo, err := lineEdits(b, o)
	if err != nil {
		return nil, err
	}
	et, err := lineEdits(b, t)
	if err != nil {
		return nil, err
	}
	et = dropIdentical(eo, et)

	var lines []string
	pos := 0
	for _, c := range clusterEdits(eo, et) {
		lines = appendLines(lines, b[pos:c.start])
		lines = appendLines(lines, c.text(b))
		pos = c.end
	}
	lines = appendLines(lines, b[pos:])
	return []byte(strings.Join(lines, "")), nil
}

// edit replaces base lines [bs, be) with repl.
type edit struct {
	bs, be int
	repl   []string
	ours   bool
}

// dropIdentical removes theirs edits that ours already makes exactly (same
// base range, same replacement). A change made identically on both sides is
// not a conflict and must appear once.
func dropIdentical(ours, theirs []edit) []edit {
	out := theirs[:0:0]
	for _, t := range theirs {
		same := false
		for _, o := range ours {
			if o.bs == t.bs && o.be == t.be && equalLines(o.repl, t.repl) {
				same = true
				break
			}
		}
		if !same {
			out = append(out, t)
		}
	}
	return out
}

// cluster is a maximal run of overlapping edits. Edits from one side only are
// applied as they are; edits from both sides that give different texts are
// a conflict, rendered as ours followed by theirs.
type cluster struct {
	start, end int
	ourEdits   []edit
	theirEdits []edit
}

func (c cluster) text(base []string) []string {
	ours := applyEdits(base, c.start, c.end, c.ourEdits)
	theirs := applyEdits(base, c.start, c.end, c.theirEdits)
	if len(c.theirEdits) == 0 {
		return ours
	}
	if len(c.ourEdits) == 0 {
		return theirs
	}
	if equalLines(ours, theirs) {
		return ours
	}
	orig := base[c.start:c.end]
	if equalLines(ours, orig) {
		return theirs
	}
	if equalLines(theirs, orig) {
		return ours
	}
	return appendLines(append([]string(nil), ours...), theirs)
}

// clusterEdits groups the edits of both sides. Two edits join a cluster when
// their base ranges overlap strictly; touching edits stay apart and are
// applied one after the other, which is a clean merge.
func clusterEdits(eo, et []edit) []cluster {
	all := make([]edit, 0, len(eo)+len(et))
	all = append(all, eo...)
	all = append(all, et...)
	for i := range all {
		all[i].ours = i < len(eo)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].bs != all[j].bs {
			return all[i].bs < all[j].bs
		}
		return all[i].be < all[j].be
	})

	var out []cluster
	for _, e := range all {
		if n := len(out); n > 0 && e.bs < out[n-1].end {
			c := &out[n-1]
			if e.be > c.end {
				c.end = e.be
			}
			addEdit(c, e)
			continue
		}
		c := cluster{start: e.bs, end: e.be}
		addEdit(&c, e)
		out = append(out, c)
	}
	return out
}

func addEdit(c *cluster, e edit) {
	if e.ours {
		c.ourEdits = append(c.ourEdits, e)
	} else {
		c.theirEdits = append(c.theirEdits, e)
	}
}

// applyEdits returns base[start:end] with the edits applied. The edits are
// from one side and do not overlap each other.
func applyEdits(base []string, start, end int, edits []edit) []string {
	sorted := append([]edit(nil), edits...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].bs < sorted[j].bs })
	var out []string
	pos := start
	for _, e := range sorted {
		out = appendLines(out, base[pos:e.bs])
		out = appendLines(out, e.repl)
		pos = e.be
	}
	return appendLines(out, base[pos:end])
}

// lineEdits returns the edits that turn base into other, computed with an
// LCS over the lines that differ after trimming the common prefix and suffix.
func lineEdits(base, other []string) ([]edit, error) {
	pre := 0
	for pre < len(base) && pre < len(other) && base[pre] == other[pre] {
		pre++
	}
	suf := 0
	for suf < len(base)-pre && suf < len(other)-pre &&
		base[len(base)-1-suf] == other[len(other)-1-suf] {
		suf++
	}
	a := base[pre : len(base)-suf]
	b := other[pre : len(other)-suf]
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil, nil
	}
	if (n+1)*(m+1) > maxUnionCells {
		return nil, errors.New("file too large to merge line by line")
	}

	// dp[i*(m+1)+j] is the LCS length of a[i:] and b[j:].
	w := m + 1
	dp := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i*w+j] = dp[(i+1)*w+j+1] + 1
			} else if dp[(i+1)*w+j] >= dp[i*w+j+1] {
				dp[i*w+j] = dp[(i+1)*w+j]
			} else {
				dp[i*w+j] = dp[i*w+j+1]
			}
		}
	}

	var edits []edit
	prevA, prevB := 0, 0
	emit := func(ai, bj int) {
		if ai > prevA || bj > prevB {
			edits = append(edits, edit{
				bs:   pre + prevA,
				be:   pre + ai,
				repl: append([]string(nil), other[pre+prevB:pre+bj]...),
			})
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			emit(i, j)
			i++
			j++
			prevA, prevB = i, j
			continue
		}
		if dp[(i+1)*w+j] >= dp[i*w+j+1] {
			i++
		} else {
			j++
		}
	}
	emit(n, m)
	return edits, nil
}

// splitLines splits b into lines that keep their newline. The last line may
// lack one.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := strings.SplitAfter(string(b), "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// appendLines appends src to dst. When dst ends with a line lacking its
// newline, the newline is added first so the pieces stay separate lines.
func appendLines(dst, src []string) []string {
	if len(src) == 0 {
		return dst
	}
	if n := len(dst); n > 0 && !strings.HasSuffix(dst[n-1], "\n") {
		dst[n-1] += "\n"
	}
	return append(dst, src...)
}

func equalLines(a, b []string) bool {
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
