package notify

import (
	"strings"
	"testing"
	"time"
)

// Characters that change what a reader sees without being seen. They are
// written as numbers so that this file stays plain ASCII.
const (
	rlo  = rune(0x202e) // RIGHT-TO-LEFT OVERRIDE
	lri  = rune(0x2066) // LEFT-TO-RIGHT ISOLATE
	pdi  = rune(0x2069) // POP DIRECTIONAL ISOLATE
	zwsp = rune(0x200b) // ZERO WIDTH SPACE
	zwj  = rune(0x200d) // ZERO WIDTH JOINER
	bom  = rune(0xfeff) // ZERO WIDTH NO-BREAK SPACE
	lsep = rune(0x2028) // LINE SEPARATOR
	psep = rune(0x2029) // PARAGRAPH SEPARATOR
)

var hiddenRunes = []rune{rlo, lri, pdi, zwsp, zwj, bom, lsep, psep}

// mixText builds a string from parts, runes and strings, so that hidden
// characters can be placed without writing them literally.
func mixText(parts ...any) string {
	var b strings.Builder
	for _, p := range parts {
		switch v := p.(type) {
		case rune:
			b.WriteRune(v)
		case string:
			b.WriteString(v)
		}
	}
	return b.String()
}

func hiddenMessage() Message {
	return Message{
		Component: mixText("pay", rlo),
		Summary:   mixText("ok ", lri, "x", pdi, "\nnext line"),
		Findings: []Finding{{
			Rule:      "revert",
			Severity:  "low",
			Summary:   mixText("revert: Fix ", rlo, "exe.txt", zwsp, bom, lsep, psep),
			Sequence:  1,
			Reference: mixText("refs/heads/a", rlo, "b"),
			Actor:     mixText("agent", zwj),
		}},
		WindowStart: time.Unix(0, 0),
		WindowEnd:   time.Unix(3600, 0),
	}
}

func TestUnprintable(t *testing.T) {
	for _, r := range append([]rune{0x1b, '\n', '\t', 0x7f, 0x85}, hiddenRunes...) {
		if !Unprintable(r) {
			t.Errorf("Unprintable(%U) = false", r)
		}
	}
	// Letters of any script, symbols and punctuation stay.
	for _, r := range []rune{'a', ' ', 0xe9, 0x20ac, 0x65e5, '\'', '`'} {
		if Unprintable(r) {
			t.Errorf("Unprintable(%U) = true", r)
		}
	}
}

func TestRenderingsRemoveFormatCharacters(t *testing.T) {
	msg := hiddenMessage()
	for name, out := range map[string]string{"text": Text(msg), "slack": SlackText(msg)} {
		for _, r := range hiddenRunes {
			if strings.ContainsRune(out, r) {
				t.Errorf("%s rendering keeps %U: %q", name, r, out)
			}
		}
		if !strings.Contains(out, "Fix  exe.txt") {
			t.Errorf("%s rendering lost the visible text: %q", name, out)
		}
		// Header, the two lines of the summary (it keeps its own newline) and the
		// finding: the separators U+2028 and U+2029 add no line.
		if got := strings.Count(out, "\n"); got != 4 {
			t.Errorf("%s rendering has %d lines, want 4: %q", name, got, out)
		}
	}
}

func TestSlackTextCannotLeaveItsCodeSpans(t *testing.T) {
	msg := Message{
		Component: "pay`*x*`",
		Summary:   "summary with `code`",
		Findings: []Finding{{
			Rule:      "revert",
			Severity:  "low",
			Summary:   "revert: a ` b",
			Sequence:  7,
			Reference: "refs/heads/x` *bold* `y",
			Actor:     "agent` _it_ `",
		}},
		WindowStart: time.Unix(0, 0),
		WindowEnd:   time.Unix(3600, 0),
	}
	out := SlackText(msg)
	// The template opens and closes four code spans: the component, the rule,
	// the reference and the actor. Any other backtick came from the message.
	if got := strings.Count(out, "`"); got != 8 {
		t.Fatalf("rendering has %d backticks, want the 8 of the template: %q", got, out)
	}
	for _, want := range []string{"`pay'*x*'`", "`refs/heads/x' *bold* 'y`", "`agent' _it_ '`", "summary with 'code'"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendering lacks %q: %q", want, out)
		}
	}
	// Links and mentions stay escaped as before.
	msg.Findings[0].Reference = "<!channel>"
	if out := SlackText(msg); strings.Contains(out, "<!channel>") || !strings.Contains(out, "&lt;!channel&gt;") {
		t.Errorf("mention not escaped: %q", out)
	}
}
