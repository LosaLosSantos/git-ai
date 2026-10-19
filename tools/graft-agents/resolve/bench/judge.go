package bench

import (
	"bytes"
	"path"
	"regexp"
	"strings"
)

// normalize returns content with line endings as LF, trailing whitespace of every line removed
// and trailing blank lines dropped: the equivalence by which a result is judged correct.
func normalize(content []byte) []byte {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return []byte(strings.Join(lines, "\n"))
}

// judge reports whether a result equals the human resolution after normalization, and whether it
// is byte-identical. A nil content is a deletion.
// blankLinesOnly reports whether two contents differ only in blank lines once normalized:
// a wrong result that is the human resolution with blank lines added or removed. It is reported
// beside the wrong count, which it stays part of; indentation and every other character count.
func blankLinesOnly(result, human []byte) bool {
	strip := func(b []byte) string {
		var out []string
		for _, line := range strings.Split(string(normalize(b)), "\n") {
			if line != "" {
				out = append(out, line)
			}
		}
		return strings.Join(out, "\n")
	}
	return strip(result) == strip(human)
}

func judge(result, human []byte, resultDeleted, humanDeleted bool) (correct, exact bool) {
	if resultDeleted || humanDeleted {
		return resultDeleted == humanDeleted, resultDeleted == humanDeleted
	}
	return bytes.Equal(normalize(result), normalize(human)), bytes.Equal(result, human)
}

// Reasons why a file is never sent to the resolver, as the server decides (PLAN10, 3.4).
const (
	NotSentBinary    = "binary"
	NotSentLarge     = "large"
	NotSentLockfile  = "lockfile"
	NotSentGenerated = "generated"
	NotSentMinified  = "minified"
)

// lockfiles are the base names of the lockfiles that always go to the agent.
var lockfiles = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"poetry.lock": true, "Cargo.lock": true, "go.sum": true,
}

// generatedMarkers are looked for in the first lines of every side.
var generatedMarkers = []*regexp.Regexp{
	regexp.MustCompile(`Code generated .* DO NOT EDIT\.`),
	regexp.MustCompile(`@generated`),
	regexp.MustCompile(`DO NOT EDIT`),
	regexp.MustCompile(`(?i)auto-generated`),
}

// generatedMarkerLines is the number of first lines searched for a marker.
const generatedMarkerLines = 10

// minifiedGlobs name minified files.
var minifiedGlobs = []string{"*.min.js", "*.min.mjs", "*.min.css", "*.bundle.js"}

// Minified thresholds: a side of more than minifiedMinBytes whose average line length is above
// minifiedAverageLine, or with a line longer than minifiedLongestLine.
const (
	minifiedMinBytes    = 2 << 10
	minifiedAverageLine = 300
	minifiedLongestLine = 5000
)

// notSentReason returns why the file at p with the given sides would not be sent to the resolver,
// or "" when it would be. generated reports linguist-generated for the path. It mirrors the
// server's SendRules.Reason (internal/graft/resolver/sendrules.go), rule for rule and in the same
// order (large first: the server decides it from the size, before reading); both are tested
// against internal/graft/resolver/testdata/never-send.json.
func notSentReason(p string, sides [][]byte, maxBytes int64, generated bool) string {
	for _, s := range sides {
		if int64(len(s)) > maxBytes {
			return NotSentLarge
		}
	}
	for _, s := range sides {
		if bytes.IndexByte(s, 0) >= 0 {
			return NotSentBinary
		}
	}
	if lockfiles[path.Base(p)] {
		return NotSentLockfile
	}
	if generated {
		return NotSentGenerated
	}
	for _, s := range sides {
		head := firstLines(s, generatedMarkerLines)
		for _, m := range generatedMarkers {
			if m.Match(head) {
				return NotSentGenerated
			}
		}
	}
	base := path.Base(p)
	for _, g := range minifiedGlobs {
		if ok, _ := path.Match(g, base); ok {
			return NotSentMinified
		}
	}
	for _, s := range sides {
		if looksMinified(s) {
			return NotSentMinified
		}
	}
	return ""
}

func firstLines(s []byte, n int) []byte {
	end := 0
	for i := 0; i < n; i++ {
		j := bytes.IndexByte(s[end:], '\n')
		if j < 0 {
			return s
		}
		end += j + 1
	}
	return s[:end]
}

// looksMinified reports whether a side larger than minifiedMinBytes has an average line longer
// than minifiedAverageLine or a line longer than minifiedLongestLine. It is the server's rule
// (internal/graft/resolver/sendrules.go): a final newline does not start another line.
func looksMinified(s []byte) bool {
	if len(s) <= minifiedMinBytes {
		return false
	}
	lines := bytes.Count(s, []byte{'\n'})
	if len(s) > 0 && s[len(s)-1] != '\n' {
		lines++
	}
	if lines > 0 && len(s)/lines > minifiedAverageLine {
		return true
	}
	for _, line := range bytes.Split(s, []byte{'\n'}) {
		if len(line) > minifiedLongestLine {
			return true
		}
	}
	return false
}
