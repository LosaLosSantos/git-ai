package resolver

import (
	"bytes"
	"fmt"
	"path"
	"regexp"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// Reasons why a conflicting file is never sent to the resolver (PLAN10 3.4). Such a file goes to
// the agent, and the reason is in the conflict package.
const (
	NotSentLarge     = "large"
	NotSentBinary    = "binary"
	NotSentLockfile  = "lockfile"
	NotSentGenerated = "generated"
	NotSentMinified  = "minified"
)

// builtinLockfiles are the base names of the lockfiles that always go to the agent.
var builtinLockfiles = []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "poetry.lock", "Cargo.lock", "go.sum"}

// builtinGeneratedMarkers are searched in the first generatedMarkerLines lines of every side.
var builtinGeneratedMarkers = []string{`Code generated .* DO NOT EDIT\.`, `@generated`, `DO NOT EDIT`, `(?i)auto-generated`}

// generatedMarkerLines is the number of first lines searched for a generated marker.
const generatedMarkerLines = 10

// builtinMinifiedGlobs are base-name globs of minified files.
var builtinMinifiedGlobs = []string{"*.min.js", "*.min.mjs", "*.min.css", "*.bundle.js"}

// minifiedMinBytes is the size above which a side is judged by its line lengths.
const minifiedMinBytes = 2 << 10

// SendRules decide which conflicting files are never sent to the resolver: large, binary,
// lockfile, generated or minified. The bench of tools/graft-agents (resolve/bench) applies the
// same rules, and both are tested against testdata/never-send.json so that they cannot drift.
type SendRules struct {
	maxFileBytes     int64
	lockfiles        map[string]bool
	markers          []*regexp.Regexp
	minifiedGlobs    []string
	averageLineBytes int
	longestLineBytes int
}

// NewSendRules returns the built-in rules extended by the configuration.
func NewSendRules(cfg config.GraftResolver) (*SendRules, error) {
	r := &SendRules{
		maxFileBytes:     cfg.MaxFileBytesOrDefault(),
		lockfiles:        map[string]bool{},
		minifiedGlobs:    append(append([]string(nil), builtinMinifiedGlobs...), cfg.Minified.Globs...),
		averageLineBytes: cfg.Minified.AverageLineBytesOrDefault(),
		longestLineBytes: cfg.Minified.LongestLineBytesOrDefault(),
	}
	for _, name := range append(append([]string(nil), builtinLockfiles...), cfg.Lockfiles...) {
		r.lockfiles[name] = true
	}
	for _, expr := range append(append([]string(nil), builtinGeneratedMarkers...), cfg.GeneratedMarkers...) {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("graft land resolver: generated marker %q: %w", expr, err)
		}
		r.markers = append(r.markers, re)
	}
	return r, nil
}

// TooLarge reports whether a side of size bytes is too large to be sent. It is checked before the
// content is read, so a large side is never read.
func (r *SendRules) TooLarge(size int64) bool {
	return size > r.maxFileBytes
}

// Reason returns why the conflicting file p with the given sides (nil for an absent side) is not
// sent, or "" when it is. generated reports linguist-generated for p in the tip's attributes.
// The rules apply in this order: large, binary, lockfile, generated (attribute, then markers),
// minified (glob, then line lengths); the first that matches is the reason.
func (r *SendRules) Reason(p string, generated bool, sides ...[]byte) string {
	for _, s := range sides {
		if r.TooLarge(int64(len(s))) {
			return NotSentLarge
		}
	}
	for _, s := range sides {
		if bytes.IndexByte(s, 0) >= 0 {
			return NotSentBinary
		}
	}
	base := path.Base(p)
	if r.lockfiles[base] {
		return NotSentLockfile
	}
	if generated {
		return NotSentGenerated
	}
	for _, s := range sides {
		head := firstLines(s, generatedMarkerLines)
		for _, m := range r.markers {
			if m.Match(head) {
				return NotSentGenerated
			}
		}
	}
	for _, g := range r.minifiedGlobs {
		if ok, _ := path.Match(g, base); ok {
			return NotSentMinified
		}
	}
	for _, s := range sides {
		if r.looksMinified(s) {
			return NotSentMinified
		}
	}
	return ""
}

// firstLines returns the first n lines of s.
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
// than averageLineBytes or a line longer than longestLineBytes.
func (r *SendRules) looksMinified(s []byte) bool {
	if len(s) <= minifiedMinBytes {
		return false
	}
	lines := bytes.Count(s, []byte{'\n'})
	if len(s) > 0 && s[len(s)-1] != '\n' {
		lines++
	}
	if lines > 0 && len(s)/lines > r.averageLineBytes {
		return true
	}
	for _, line := range bytes.Split(s, []byte{'\n'}) {
		if len(line) > r.longestLineBytes {
			return true
		}
	}
	return false
}
