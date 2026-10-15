package notify

import (
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestTextRemovesControlCharacters(t *testing.T) {
	msg := Message{
		Component: "pay\x1b[2J",
		Summary:   "line\x1b]0;title\x07",
		Findings: []Finding{{
			Rule:      "revert",
			Severity:  "low",
			Summary:   "revert: sub\x1b[31mject\nsecond line",
			Sequence:  1,
			Reference: "refs/heads/x\x1b",
			Actor:     "a\x07",
		}},
		WindowStart: time.Unix(0, 0),
		WindowEnd:   time.Unix(3600, 0),
	}
	out := Text(msg)
	for _, r := range out {
		if r != '\n' && unicode.IsControl(r) {
			t.Fatalf("control character %U in the rendering: %q", r, out)
		}
	}
	// The newline of a finding would split the finding line in two, so it is
	// replaced: header, summary and finding, three lines.
	if got := strings.Count(out, "\n"); got != 3 {
		t.Errorf("rendering has %d lines, want 3: %q", got, out)
	}
	if !strings.Contains(out, "revert: sub [31mject second line") {
		t.Errorf("finding text not preserved in plain form: %q", out)
	}
}
