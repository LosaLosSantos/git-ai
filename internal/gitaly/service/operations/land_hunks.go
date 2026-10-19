package operations

import (
	"github.com/feder-cr/git-ai/internal/graft/merge3"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// landConflictHunks returns the conflicting regions of a three-way merge of base, ours and theirs
// (merge3) as the hunks of the conflict package: line ranges of each side, 1-based and half-open,
// each counting the lines of its own side. It returns nil when a diff exceeds merge3.MaxCells; the
// agent still has the sides.
func landConflictHunks(base, ours, theirs []byte) []*gitalypb.LandConflictHunk {
	result, ok := merge3.Merge(base, ours, theirs)
	if !ok {
		return nil
	}
	var hunks []*gitalypb.LandConflictHunk
	for _, h := range result.Hunks() {
		hunks = append(hunks, &gitalypb.LandConflictHunk{
			Base:   landRange(h.Base),
			Ours:   landRange(h.Ours),
			Theirs: landRange(h.Theirs),
		})
	}
	return hunks
}

// landRange returns the 1-based half-open range of the 0-based range r.
func landRange(r merge3.Range) *gitalypb.LineRange {
	return &gitalypb.LineRange{Start: uint32(r.Start + 1), End: uint32(r.End + 1)}
}
