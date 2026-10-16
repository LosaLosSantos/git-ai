package superfan

import (
	"strings"

	"github.com/feder-cr/git-ai/tools/graft-agents/notify"
)

// Bounds on the text that reaches findings and digests. Commit subjects,
// trailers, paths and actors come from commits and clients, so they are
// untrusted: they are cut to these sizes and stripped of control and format
// characters (escape sequences and bidirectional overrides included, see
// notify.Unprintable) before they are shown anywhere.
const (
	// maxSummaryText bounds the summary of one finding.
	maxSummaryText = 512
	// maxLabelText bounds short labels such as actors and operations.
	maxLabelText = 256
	// maxPathText bounds one path shown in a summary or a digest entry.
	maxPathText = 256
	// maxSubjectText bounds the commit subject kept in a digest entry.
	maxSubjectText = 256
	// maxFindingPaths bounds the paths kept in one finding.
	maxFindingPaths = 20
	// maxEntryFiles bounds the files listed for one changeset in a digest.
	maxEntryFiles = 10
	// maxDigestEntries bounds the changesets listed in one digest.
	maxDigestEntries = 100
)

// oneLine returns s as one line of at most max bytes. Every control or format
// character, newlines, escape sequences and bidirectional overrides included,
// becomes a space.
func oneLine(s string, max int) string {
	return clean(s, max, false)
}

// multiLine is oneLine except that newlines are kept, for text that is
// meant to span lines, such as a summary.
func multiLine(s string, max int) string {
	return clean(s, max, true)
}

func clean(s string, max int, keepNewlines bool) string {
	// Only the prefix that can survive truncation is processed, so that a huge
	// input costs a bounded amount of work.
	if len(s) > 4*max {
		s = s[:4*max]
	}
	var b strings.Builder
	for _, r := range s {
		if keepNewlines && r == '\n' {
			b.WriteRune(r)
			continue
		}
		if notify.Unprintable(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return truncateText(b.String(), max)
}
