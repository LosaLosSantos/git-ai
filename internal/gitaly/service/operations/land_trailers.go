package operations

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The Graft-* trailers of a resolution commit. graft-verify and superfan read them back, so every
// value must stay one trailer line: landResolutionTrailers is the only place that writes them.
const (
	landTrailerResolvedBy   = "Graft-Resolved-By"
	landTrailerCheckedBy    = "Graft-Checked-By"
	landTrailerResolvedPath = "Graft-Resolved-Path"
)

// landTrailerValueValid reports whether v can be the value of a commit trailer as it is: non-empty,
// valid UTF-8, and printable. unicode.IsPrint admits the ASCII space only, so line breaks (also
// U+0085, U+2028 and U+2029), NUL, tab, escape sequences and format characters are all refused: a
// value can never start another trailer or hide part of itself.
func landTrailerValueValid(v string) bool {
	if v == "" || !utf8.ValidString(v) {
		return false
	}
	for _, r := range v {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// landTrailerUnsafeError is the refusal of a value that landTrailerValueValid rejects.
type landTrailerUnsafeError struct {
	trailer, value string
}

func (e *landTrailerUnsafeError) Error() string {
	return fmt.Sprintf("%s %q cannot be written as a commit trailer: it must be one line of printable UTF-8", e.trailer, e.value)
}

// landResolutionTrailers returns the trailer block of a resolution commit: Graft-Resolved-By with
// the resolver's name, Graft-Checked-By with the name of the resolver that checked an agent's
// resolution (when one did), and one Graft-Resolved-Path per resolved path. It refuses, with a
// *landTrailerUnsafeError, any value that landTrailerValueValid rejects; nothing is ever escaped.
func landResolutionTrailers(resolverName, checkedBy string, paths []string) (string, error) {
	var b strings.Builder
	add := func(trailer, value string) error {
		if !landTrailerValueValid(value) {
			return &landTrailerUnsafeError{trailer: trailer, value: value}
		}
		b.WriteString(trailer + ": " + value + "\n")
		return nil
	}

	if err := add(landTrailerResolvedBy, resolverName); err != nil {
		return "", err
	}
	if checkedBy != "" {
		if err := add(landTrailerCheckedBy, checkedBy); err != nil {
			return "", err
		}
	}
	for _, p := range paths {
		if err := add(landTrailerResolvedPath, p); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}
