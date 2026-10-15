package superfan

import (
	"errors"
	"path"
	"strings"
)

// maxGlobLength and maxGlobDoubleStars bound patterns so that matching stays
// cheap whatever the configuration says.
const (
	maxGlobLength     = 512
	maxGlobDoubleStar = 4
)

// ValidPath checks a repository path: relative, clean, no ".." segment, no
// backslash and no control characters. It is stricter than git, which accepts
// backslashes, tabs and newlines in paths, so it is not applied to the paths of
// commits: ParseDetails keeps every path and the rules match them with
// MatchFile, so that an odd path cannot hide a commit from the rules.
func ValidPath(p string) error {
	switch {
	case p == "":
		return errors.New("empty path")
	case len(p) > 4096:
		return errors.New("path too long")
	case strings.HasPrefix(p, "/"):
		return errors.New("absolute path")
	case strings.ContainsAny(p, "\\\x00\n\r\t"):
		return errors.New("path contains a forbidden character")
	case path.Clean(p) != p || p == ".":
		return errors.New("path is not clean")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return errors.New("path escapes the repository")
		}
	}
	return nil
}

// ValidGlob checks a path pattern used by MatchPath.
func ValidGlob(pattern string) error {
	if pattern == "" {
		return errors.New("empty pattern")
	}
	if len(pattern) > maxGlobLength {
		return errors.New("pattern too long")
	}
	if strings.HasPrefix(pattern, "/") || strings.HasSuffix(pattern, "/") || strings.Contains(pattern, "//") {
		return errors.New("pattern must not start or end with / or contain //")
	}
	doubles := 0
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			doubles++
		}
		if seg == ".." {
			return errors.New("pattern must not contain ..")
		}
		if _, err := path.Match(seg, ""); err != nil {
			return errors.New("malformed pattern")
		}
	}
	if doubles > maxGlobDoubleStar {
		return errors.New("too many ** segments")
	}
	return nil
}

// MatchPath reports whether name matches pattern. Segments are separated by
// "/". Within a segment "*", "?" and "[...]" have path.Match semantics. A
// segment "**" matches zero or more whole segments, so "internal/pay/**"
// matches "internal/pay/x.go", "internal/pay/a/b.go" and "internal/pay".
// An invalid pattern matches nothing.
func MatchPath(pattern, name string) bool {
	if ValidGlob(pattern) != nil {
		return false
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

// matchSegments matches the segments of a pattern against the segments of a
// path. Without "**" the match is one comparison per segment. With "**" it is
// a dynamic program over (pattern segment, path segment) pairs, so the cost is
// O(len(pat) * len(name)) however many "**" segments the pattern has. A
// backtracking matcher would be exponential in their number.
func matchSegments(pat, name []string) bool {
	if !hasDoubleStar(pat) {
		if len(pat) != len(name) {
			return false
		}
		for i := range pat {
			if !segmentMatch(pat[i], name[i]) {
				return false
			}
		}
		return true
	}

	m, n := len(pat), len(name)
	// cur[j] reports whether pat[i:] matches name[j:], next[j] whether pat[i+1:]
	// does. Rows are filled from the end, so every cell reads only cells already
	// filled. Two rows are enough, so the memory is O(len(name)) however long
	// the path is: paths come from commits and are not bounded by this package.
	next := make([]bool, n+1)
	cur := make([]bool, n+1)
	next[n] = true // the empty pattern matches the empty rest of the path
	for i := m - 1; i >= 0; i-- {
		for j := n; j >= 0; j-- {
			switch {
			case pat[i] == "**":
				// Matches zero segments, or one more segment of the path.
				cur[j] = next[j] || (j < n && cur[j+1])
			case j < n && segmentMatch(pat[i], name[j]):
				cur[j] = next[j+1]
			default:
				cur[j] = false
			}
		}
		next, cur = cur, next
	}
	return next[0]
}

func hasDoubleStar(pat []string) bool {
	for _, seg := range pat {
		if seg == "**" {
			return true
		}
	}
	return false
}

// segmentMatch matches one path segment with path.Match semantics.
func segmentMatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

// matchAny reports whether name matches at least one pattern.
func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if MatchPath(p, name) {
			return true
		}
	}
	return false
}

// MatchFile reports whether a changed file of a commit matches at least one
// path pattern. The path comes from a commit, so it is untrusted and may be
// anything git can store: a backslash, a tab or a newline is legal in a git
// path. Such a path is never a reason to skip a rule. It is matched as it is
// and also in a normalized form (backslashes read as separators, then cleaned
// as an absolute path made relative), so that an odd spelling of a protected
// path such as "ledger\keys.go", "./ledger/keys.go" or "ledger//keys.go" still
// matches "ledger/**". Matching more than git would check out is the safe side
// for a monitor. Display is a separate concern (see oneLine).
func MatchFile(patterns []string, p string) bool {
	if matchAny(patterns, p) {
		return true
	}
	if alt := normalizePath(p); alt != p && alt != "" {
		return matchAny(patterns, alt)
	}
	return false
}

// normalizePath returns p with backslashes read as separators, cleaned, and
// without a leading slash. It may be empty.
func normalizePath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(p, `\`, "/")), "/")
}
