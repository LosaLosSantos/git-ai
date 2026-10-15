package classify

import (
	"testing"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// nbsp and bom are the UTF-8 encodings of U+00A0 (no-break space) and U+FEFF
// (zero width no-break space, byte order mark), written as byte escapes so the
// source stays plain ASCII.
const (
	nbsp = "\xc2\xa0"
	bom  = "\xef\xbb\xbf"
)

// Regression tests for the round 2 review of the classifier. Each case is a
// change that alters the program and must not be COMMENT_ONLY, or a change that
// is only a comment and must stay COMMENT_ONLY.
func TestRegressionRound2(t *testing.T) {
	const (
		code    = gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED
		comment = gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY
	)
	cases := []struct {
		name string
		path string
		old  string
		new  string
		want gitalypb.ReviewFile_ChangeClass
	}{
		// C: a line splice continues a line comment onto the next line.
		{"C line comment ending in backslash", "a.c",
			"int x = 7;\nint main(void) {\n  // note \\\n  x = 1;\n  return x;\n}\n",
			"int x = 7;\nint main(void) {\n  // note\n  x = 1;\n  return x;\n}\n",
			code},
		{"C splice with trailing blank", "a.c",
			"int a; // x\\ \t\r\nint b;\n", "int a; // y\nint b;\n", code},
		{"C trigraph splice", "a.c", "int a; // x ??/\nint b;\n", "int a; // y ??/\nint b;\n", code},
		// C: blanks inside a preprocessor line decide function-like macros.
		{"C blank between macro name and paren", "a.c",
			"typedef int x;\n#define P(x) 1*\nint a = P(2)+5;\n",
			"typedef int x;\n#define P (x) 1*\nint a = P(2)+5;\n",
			code},
		{"C comment between macro name and paren", "a.c",
			"#define P(x) 1\nint a = P(2);\n", "#define P/**/(x) 1\nint a = P(2);\n", code},
		{"C stringification spacing", "a.c",
			"#define S(x) #x\nchar *s = S(a+b);\n", "#define S(x) #x\nchar *s = S(a + b);\n", code},
		{"C digraph directive", "a.c", "%:define P(x) 1\n", "%:define P (x) 1\n", code},
		{"C trigraph directive", "a.c", "??=define P(x) 1\n", "??=define P (x) 1\n", code},
		// PLAN5 "WP44 RULE" 1: C is never COMMENT_ONLY.
		{"C comment in preprocessor line is code", "a.c",
			"#define A 1 // a\nint x;\n", "#define A 1 // b\nint x;\n", code},
		// JavaScript: regular expressions after keywords and non-ASCII blanks.
		{"JS regex after export default", "a.js",
			"export default /[//]/.source + \"a\";\n",
			"export default /[//]/.source + \"b\";\n",
			code},
		{"JS regex after return and NBSP", "a.js",
			"function f(){return" + nbsp + "/[//]/.source+\"a\"}\n",
			"function f(){return" + nbsp + "/[//]/.source+\"b\"}\n",
			code},
		{"JS regex after typeof and BOM", "a.js",
			"function f(){return typeof" + bom + "/[//]/.source+\"a\"}\n",
			"function f(){return typeof" + bom + "/[//]/.source+\"b\"}\n",
			code},
		// PLAN5 "WP44 RULE" 1: JavaScript is never COMMENT_ONLY.
		{"JS NBSP between tokens is code", "a.js",
			"x" + nbsp + "=" + nbsp + "1;\n", "x = 1;\n", code},
		// Go: an import of "C" may be a raw string literal.
		{"cgo raw string import", "p/a.go",
			"package p\n\n/*\nint f(void) { return 1; }\n*/\nimport `C`\n",
			"package p\n\n/*\nint f(void) { return 2; }\n*/\nimport `C`\n",
			code},
		// Ruby: percent literals with '#' or blank delimiters.
		{"Ruby percent literal with hash delimiter", "a.rb",
			"puts %#hello#\n", "puts %#world#\n", code},
		{"Ruby percent with blank delimiter", "a.rb",
			"x = % #a \nputs x\n", "x = % #b \nputs x\n", code},
		// Ruby: blanks around unary operators and brackets.
		{"Ruby unary minus after method name", "a.rb",
			"def f(x = 10) x end\ny = f -1\n", "def f(x = 10) x end\ny = f - 1\n", code},
		{"Ruby bracket after name", "a.rb",
			"a = [1]\nb = a [0]\n", "a = [1]\nb = a[0]\n", code},
		// PLAN5 "WP44 RULE" 1: Ruby is never COMMENT_ONLY.
		{"Ruby comment changed after operators", "a.rb",
			"a = b + c # x\n", "a = b + c # y\n", code},
		{"Ruby names only, blank changed", "a.rb",
			"puts x\n", "puts  x\n", code},
		// Rust: doc comments are attributes.
		{"Rust line doc comment", "a.rs", "/// old help\nfn f() {}\n", "/// new help\nfn f() {}\n", code},
		{"Rust block doc comment", "a.rs", "/** old */\nfn f() {}\n", "/** new */\nfn f() {}\n", code},
		{"Rust four slashes is a plain comment", "a.rs", "//// a\nfn f() {}\n", "//// b\nfn f() {}\n", comment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := File(tc.path, []byte(tc.old), []byte(tc.new))
			if got != tc.want {
				t.Fatalf("File class = %v, want %v (reason %q)", got, tc.want, reason)
			}
		})
	}
}

// TestRegressionRound3 covers every repro of the third gate (PLAN5 "WP44
// RULE"): each must be CODE. The positive cases check that the four supported
// languages still recognise comment-only changes.
func TestRegressionRound3(t *testing.T) {
	const (
		code    = gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED
		comment = gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY
	)
	cases := []struct {
		name string
		path string
		old  string
		new  string
		want gitalypb.ReviewFile_ChangeClass
	}{
		// Rule 1: only Go, Python, Rust and Java may be COMMENT_ONLY.
		{"C block comment", "src/a.c", "/* a */ int x;\n", "/* b */ int x;\n", code},
		{"C header comment", "src/a.h", "// a\nint x;\n", "// b\nint x;\n", code},
		{"C++ comment", "src/a.cpp", "// a\nint x;\n", "// b\nint x;\n", code},
		{"JavaScript comment", "web/a.js", "// a\nconst x = 1;\n", "// b\nconst x = 1;\n", code},
		{"TypeScript comment", "web/a.ts", "// a\nconst x = 1;\n", "// b\nconst x = 1;\n", code},
		{"shell comment", "run.sh", "# a\necho hi\n", "# b\necho hi\n", code},
		{"Ruby comment", "src/a.rb", "# a\nx = 1\n", "# b\nx = 1\n", code},
		// Rule 1: Javadoc is code; plain block comments are not.
		{"Javadoc changed", "src/Main.java", "/** a */\nclass A {}\n", "/** b */\nclass A {}\n", code},
		{"Java plain block comment", "src/Main.java", "/* a */\nclass A {}\n", "/* b */\nclass A {}\n", comment},
		// Rule 1: a #cgo line in Go is code even without import "C".
		{"Go #cgo line without import C", "pkg/a.go", "package a\n// #cgo CFLAGS: -DX\n", "package a\n// #cgo CFLAGS: -DY\n", code},
		{"Go #cgo in block comment", "pkg/a.go", "package a\n/*\n#cgo LDFLAGS: -lx\n*/\n", "package a\n/*\n#cgo LDFLAGS: -ly\n*/\n", code},
		// Rule 2: invalid UTF-8 gives CODE.
		{"Go invalid UTF-8 in a string", "pkg/a.go", "package a\n// a\nvar s = \"\xff\"\n", "package a\n// b\nvar s = \"\xff\"\n", code},
		{"Python invalid UTF-8 in a comment", "a.py", "x = 1  # \xff\n", "x = 1  # a\n", code},
		// Rule 3: a generated marker added to an existing file does not count.
		{"Code generated marker added", "pkg/a.go",
			"package a\nfunc f() int { return 1 }\n",
			"// Code generated by x. DO NOT EDIT.\npackage a\nfunc f() int { return 2 }\n", code},
		{"@generated marker added", "pkg/a.go",
			"package a\nfunc f() int { return 1 }\n",
			"// @generated\npackage a\nfunc f() int { return 2 }\n", code},
		// Positive cases for the supported languages.
		{"Go comment", "pkg/a.go", "package a\n// a\nvar x = 1\n", "package a\n// b\nvar x = 1\n", comment},
		{"Python comment", "pkg/a.py", "# a\nx = 1\n", "# b\nx = 1\n", comment},
		{"Rust comment", "src/a.rs", "// a\nfn f() {}\n", "// b\nfn f() {}\n", comment},
		{"Java comment", "src/A.java", "// a\nclass A {}\n", "// b\nclass A {}\n", comment},
		{"Java reformatted", "src/A.java", "class A { int x; }\n", "class A {\n    int x;\n}\n", comment},
		{"Go UTF-8 string unchanged", "pkg/a.go", "package a\n// a\nvar s = \"\xc3\xa9\"\n", "package a\n// b\nvar s = \"\xc3\xa9\"\n", comment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := File(tc.path, []byte(tc.old), []byte(tc.new))
			if got != tc.want {
				t.Fatalf("File class = %v, want %v (reason %q)", got, tc.want, reason)
			}
		})
	}
}
