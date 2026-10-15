// Package notify delivers superfan digests to people and systems: stdout, a
// JSON webhook and a Slack incoming webhook. A Notifier only sends. It never
// decides what is worth sending; that is package superfan's job.
package notify

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// KindDigest is the Kind of a digest message.
const KindDigest = "digest"

// Message is one digest of one component for one time window. Its JSON form is
// the webhook payload documented in doc/graft/SUPERFAN.md ("Digests"): keep the
// two in step.
type Message struct {
	Kind        string    `json:"kind"`
	Component   string    `json:"component"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	// Events is the number of changesets of the component in the window.
	Events int `json:"events"`
	// Risk is the highest risk level of the findings: none, low, medium or high.
	Risk string `json:"risk"`
	// Summary is the optional summary text of a summarizer.
	Summary  string    `json:"summary,omitempty"`
	Findings []Finding `json:"findings"`
	// Omitted counts findings that did not fit in Findings.
	Omitted int `json:"omitted,omitempty"`
}

// Finding is one observation about a changeset.
type Finding struct {
	Rule       string    `json:"rule"`
	Severity   string    `json:"severity"`
	Summary    string    `json:"summary"`
	Sequence   uint64    `json:"sequence"`
	Reference  string    `json:"reference"`
	Actor      string    `json:"actor,omitempty"`
	OID        string    `json:"oid,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Notifier delivers messages to one destination.
type Notifier interface {
	// Name is the configured name of the destination.
	Name() string
	// Notify delivers msg. It returns an error when delivery failed.
	Notify(ctx context.Context, msg Message) error
}

// Text renders msg as plain text, one finding per line. Every field that holds
// text from commits or clients is stripped of unprintable characters first (see
// Unprintable), so that an escape sequence or a bidirectional override in a
// commit subject cannot reach a terminal.
func Text(msg Message) string {
	var b strings.Builder
	kind := msg.Kind
	if kind == "" {
		kind = KindDigest
	}
	fmt.Fprintf(&b, "[%s] component=%s window=%s..%s events=%d risk=%s\n",
		kind, printable(msg.Component, false),
		msg.WindowStart.UTC().Format(time.RFC3339), msg.WindowEnd.UTC().Format(time.RFC3339),
		msg.Events, printable(msg.Risk, false))
	if msg.Summary != "" {
		fmt.Fprintf(&b, "%s\n", printable(msg.Summary, true))
	}
	for _, f := range msg.Findings {
		fmt.Fprintf(&b, "- [%s/%s] %s (seq %d, %s", printable(f.Severity, false), printable(f.Rule, false),
			printable(f.Summary, false), f.Sequence, printable(f.Reference, false))
		if f.Actor != "" {
			fmt.Fprintf(&b, ", actor %s", printable(f.Actor, false))
		}
		b.WriteString(")\n")
	}
	if msg.Omitted > 0 {
		fmt.Fprintf(&b, "... %d more findings omitted\n", msg.Omitted)
	}
	return b.String()
}

// printable replaces every unprintable character of s (see Unprintable),
// escape sequences included, by a space. Newlines are kept when keepNewlines
// is set.
func printable(s string, keepNewlines bool) string {
	return strings.Map(func(r rune) rune {
		if keepNewlines && r == '\n' {
			return r
		}
		if Unprintable(r) {
			return ' '
		}
		return r
	}, s)
}

// Unprintable reports whether r must not reach a terminal or a chat message
// as it is, because it is text from commits or clients: control characters
// (escape sequences, newlines, tabs), format characters (Unicode category Cf:
// bidirectional overrides and isolates such as U+202E, which can reorder what
// a reader sees, zero-width characters, the byte order mark) and the line and
// paragraph separators U+2028 and U+2029 (categories Zl and Zp).
func Unprintable(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}
