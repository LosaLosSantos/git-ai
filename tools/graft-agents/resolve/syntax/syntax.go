// Package syntax checks that a resolved file still parses, with the tree-sitter grammar of its
// language. It is the syntax check of PLAN10 (3.2): any ERROR or MISSING node in the tree rejects
// the content, with the position of the first one. A path whose extension has no grammar here is
// not checked (the marker rule, which every strategy gets, still applies to it).
//
// The grammars are compiled from C with cgo, so graft-resolve needs a C compiler to build
// (CGO_ENABLED=1). Built without cgo, the package fails closed: Check rejects the content of
// every path that has a grammar with ErrUnavailable, so no answer is taken unchecked.
package syntax

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
)

// Language names, as Check reports them.
const (
	TypeScript = "typescript"
	TSX        = "tsx"
	JavaScript = "javascript"
	Python     = "python"
	Go         = "go"
	Rust       = "rust"
	Java       = "java"
)

var extensions = map[string]string{
	".ts": TypeScript, ".mts": TypeScript, ".cts": TypeScript,
	".tsx": TSX,
	".js":  JavaScript, ".mjs": JavaScript, ".cjs": JavaScript, ".jsx": JavaScript,
	".py": Python, ".pyi": Python,
	".go":   Go,
	".rs":   Rust,
	".java": Java,
}

// ErrUnavailable is the error of Check, for a path with a grammar, in a build without cgo.
var ErrUnavailable = errors.New("syntax check: unavailable, graft-resolve was built without cgo")

// LanguageOf returns the language whose grammar checks p, chosen by its extension (case does not
// matter), and false when there is none.
func LanguageOf(p string) (string, bool) {
	lang, ok := extensions[strings.ToLower(path.Ext(p))]
	return lang, ok
}

// Error is a syntax error in checked content.
type Error struct {
	Path     string
	Language string
	// Line and Column are 1-based; Column counts bytes.
	Line, Column int
	// Missing is the kind of the node the parser had to insert, empty for an ERROR node (text
	// that fits nowhere in the grammar).
	Missing string
}

func (e *Error) Error() string {
	what := "a syntax error"
	if e.Missing != "" {
		what = fmt.Sprintf("a missing %q", e.Missing)
	}
	return fmt.Sprintf("syntax check: %s does not parse as %s: %s at line %d, column %d", e.Path, e.Language, what, e.Line, e.Column)
}

// Check parses content as the language of p and returns an *Error for the first ERROR or MISSING
// node of the tree. It returns nil when content parses, and when p has no grammar. It fails with
// ctx's error when ctx ends during the parse.
func Check(ctx context.Context, p string, content []byte) error {
	lang, ok := LanguageOf(p)
	if !ok {
		return nil
	}
	return parse(ctx, lang, p, content)
}
