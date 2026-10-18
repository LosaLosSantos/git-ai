// Package syntax checks that a resolved file still parses, with the tree-sitter grammar of its
// language. It is the syntax check of PLAN10 (3.2): any ERROR or MISSING node in the tree rejects
// the content, with the position of the first one. A path whose extension has no grammar here is
// not checked (the marker rule, which every strategy gets, still applies to it).
//
// The grammars are compiled from C with cgo, so graft-resolve needs a C compiler to build
// (CGO_ENABLED=1).
package syntax

import (
	"context"
	"fmt"
	"path"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	golang "github.com/tree-sitter/tree-sitter-go/bindings/go"
	java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
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

var grammars = map[string]*sitter.Language{
	TypeScript: sitter.NewLanguage(typescript.LanguageTypescript()),
	TSX:        sitter.NewLanguage(typescript.LanguageTSX()),
	JavaScript: sitter.NewLanguage(javascript.Language()),
	Python:     sitter.NewLanguage(python.Language()),
	Go:         sitter.NewLanguage(golang.Language()),
	Rust:       sitter.NewLanguage(rust.Language()),
	Java:       sitter.NewLanguage(java.Language()),
}

var extensions = map[string]string{
	".ts": TypeScript, ".mts": TypeScript, ".cts": TypeScript,
	".tsx": TSX,
	".js":  JavaScript, ".mjs": JavaScript, ".cjs": JavaScript, ".jsx": JavaScript,
	".py": Python, ".pyi": Python,
	".go":   Go,
	".rs":   Rust,
	".java": Java,
}

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

	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(grammars[lang]); err != nil {
		return fmt.Errorf("syntax check: %s grammar: %w", lang, err)
	}

	n := len(content)
	tree := parser.ParseWithOptions(func(i int, _ sitter.Point) []byte {
		if i < n {
			return content[i:]
		}
		return nil
	}, nil, &sitter.ParseOptions{ProgressCallback: func(sitter.ParseState) bool { return ctx.Err() != nil }})
	if tree == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("syntax check: %s: the %s parser gave no tree", p, lang)
	}
	defer tree.Close()

	root := tree.RootNode()
	if !root.HasError() {
		return nil
	}
	bad := firstError(root)
	if bad == nil {
		// HasError without an ERROR or MISSING node below cannot happen; reject anyway.
		bad = root
	}
	pos := bad.StartPosition()
	e := &Error{Path: p, Language: lang, Line: int(pos.Row) + 1, Column: int(pos.Column) + 1}
	if bad.IsMissing() {
		e.Missing = bad.Kind()
	}
	return e
}

// firstError returns the first ERROR or MISSING node in document order, descending only into
// nodes that contain an error.
func firstError(root *sitter.Node) *sitter.Node {
	stack := []*sitter.Node{root}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if node.IsError() || node.IsMissing() {
			return node
		}
		for i := node.ChildCount(); i > 0; i-- {
			if child := node.Child(i - 1); child != nil && child.HasError() {
				stack = append(stack, child)
			}
		}
	}
	return nil
}
