package main

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// displayText returns s in a form that a terminal shows as text. Text that the
// server reports, such as actors and resolved paths, comes from other clients
// and from file names, which may contain any byte but NUL and slash. Printed as
// it is, an escape sequence in it would act on the terminal of whoever runs
// graft: it could move the cursor and overwrite earlier lines, for example to
// forge which actor landed which commit of a batch.
//
// Every character that is not shown as itself is written as a Go escape:
// control characters (\x1b, \x09 for a tab, \u0085), format characters such as the
// bidirectional override U+202E, the line and paragraph separators U+2028 and
// U+2029, and bytes that are not valid UTF-8 (\xff). Everything else is kept.
// --json output carries the exact text instead.
func displayText(s string) string {
	if strings.IndexFunc(s, needsEscape) < 0 && utf8.ValidString(s) {
		return s
	}

	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case !needsEscape(r):
			b.WriteRune(r)
		case r < utf8.RuneSelf:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
		i += size
	}

	return b.String()
}

// displayList returns the items as one comma-separated line, each made safe
// with displayText.
func displayList(items []string) string {
	shown := make([]string, len(items))
	for i, item := range items {
		shown[i] = displayText(item)
	}

	return strings.Join(shown, ", ")
}

// needsEscape reports whether r must not reach a terminal as it is: a control
// character, a format character (Unicode category Cf) or a line or paragraph
// separator (Zl, Zp).
func needsEscape(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}
