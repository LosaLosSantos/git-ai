//go:build cgo

package syntax

import (
	"context"
	"fmt"

	sitter "github.com/tree-sitter/go-tree-sitter"
	golang "github.com/tree-sitter/tree-sitter-go/bindings/go"
	java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Available reports whether this build parses (it was built with cgo).
const Available = true

var grammars = map[string]*sitter.Language{
	TypeScript: sitter.NewLanguage(typescript.LanguageTypescript()),
	TSX:        sitter.NewLanguage(typescript.LanguageTSX()),
	JavaScript: sitter.NewLanguage(javascript.Language()),
	Python:     sitter.NewLanguage(python.Language()),
	Go:         sitter.NewLanguage(golang.Language()),
	Rust:       sitter.NewLanguage(rust.Language()),
	Java:       sitter.NewLanguage(java.Language()),
}

func parse(ctx context.Context, lang, p string, content []byte) error {
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
