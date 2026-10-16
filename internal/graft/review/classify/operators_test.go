package classify

import (
	"testing"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestOperatorsAreLanguageExact checks that each C-like lexer splits a run of
// operator characters with its own language's operators. An operator borrowed
// from another language merges characters that the language splits, so two
// spellings that differ to the compiler lex alike. FuzzGoMatchesGoScanner found
// ">>> >" and ">>>>" classified COMMENT_ONLY in Go because Java's ">>>" was in
// a list shared by all three languages; go/scanner reads ">>" ">" ">" and ">>"
// ">>".
func TestOperatorsAreLanguageExact(t *testing.T) {
	t.Parallel()

	const (
		code    = gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED
		comment = gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY
	)

	for _, tc := range []struct {
		name     string
		path     string
		old, new string
		want     gitalypb.ReviewFile_ChangeClass
	}{
		{"go: Java unsigned shift is not a Go operator", "a.go", ">>> >", ">>>>", code},
		{"go: Java unsigned shift assignment", "a.go", "x >>>= 1\n", "x >>> = 1\n", code},
		{"go: split like go/scanner", "a.go", "x >>>= 1\n", "x >> >= 1\n", comment},
		{"go: Rust path separator is not a Go operator", "a.go", "a ::= b\n", "a :: = b\n", code},
		{"go: JavaScript strict equality is not a Go operator", "a.go", "a === =b\n", "a ====b\n", code},
		{"go: Go operators still join", "a.go", "x := a &^ b // c\n", "x := a &^ b\n", comment},
		{"go: blank inside a Go operator", "a.go", "x := a &^ b\n", "x := a & ^b\n", code},
		{"go: a leading dot belongs to a float literal", "a.go", "x := .5\n", "x := . 5\n", code},
		{"go: blank before a float with a leading dot", "a.go", "x := .5 // c\n", "x :=   .5\n", comment},
		{"java: a leading dot belongs to a float literal", "A.java", "double x = .5;", "double x = . 5;", code},
		{"rust: blanks around a tuple field dot", "a.rs", "let x = t.0; // c\n", "let x = t. 0;\n", comment},
		{"java: Go define is not a Java operator", "A.java", "a ? b :== c;", "a ? b := = c;", code},
		{"java: unsigned shift stays one operator", "A.java", "x >>>= 1; // c", "x >>>= 1;", comment},
		{"java: blank inside unsigned shift", "A.java", "x = y >>> 1;", "x = y >> > 1;", code},
		{"rust: no decrement operator", "a.rs", "a -->b;", "a -- >b;", code},
		{"rust: range operators", "a.rs", "let r = 0..=9; // c\n", "let r = 0..=9;\n", comment},
		{"rust: blank inside a range operator", "a.rs", "let r = a..=b;\n", "let r = a.. =b;\n", code},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, reason := File(tc.path, []byte(tc.old), []byte(tc.new))
			if got != tc.want {
				t.Fatalf("File(%q) = %v (%s), want %v", tc.path, got, reason, tc.want)
			}
		})
	}
}
