package classify

import (
	"strings"
	"testing"
)

// TestRegressUnsoundLexing covers the repros of the adversarial review: each
// case hides a code change behind a comment or string lookalike, so it must be
// CODE. The controls at the end must stay COMMENT_ONLY. C, JavaScript, shell and
// Ruby are no longer lexed at all (PLAN5 "WP44 RULE" 1), so their repros are
// CODE by extension.
func TestRegressUnsoundLexing(t *testing.T) {
	runFileCases(t, []fileCase{
		// JavaScript regex versus division.
		{"regex after paren hides comment marker", "a.js", "if (x) /[//]/.test(good)\n", "if (x) /[//]/.test(evil)\n", clsCode},
		{"division after brace", "a.js", "x = {} / 2 + \"/ //\"; good()\n", "x = {} / 2 + \"/ //\"; evil()\n", clsCode},
		{"contextual keyword as identifier", "a.js", "x = of / 2 + \"/ //\"; good()\n", "x = of / 2 + \"/ //\"; evil()\n", clsCode},
		{"property named like keyword is division", "a.js", "x = a.return / 2 + \"/ //\"; good()\n", "x = a.return / 2 + \"/ //\"; evil()\n", clsCode},
		{"division after identifier is code too", "a.js", "a = b / 2; // x\n", "a = b / 2; // y\n", clsCode},

		// Line terminators other than newline.
		{"lone CR in C comment", "a.c", "int a = 1; // x\rgood();\n", "int a = 1; // x\revil();\n", clsCode},
		{"lone CR in C++ comment", "a.cc", "int a = 1; // x\rgood();\n", "int a = 1; // x\revil();\n", clsCode},
		{"lone CR in Java comment", "A.java", "int a = 1; // x\rgood();\n", "int a = 1; // x\revil();\n", clsCode},
		{"lone CR in Go comment", "a.go", "a := 1 // x\rgood()\n", "a := 1 // x\revil()\n", clsCode},
		{"lone CR in Rust comment", "a.rs", "let a = 1; // x\rgood();\n", "let a = 1; // x\revil();\n", clsCode},
		{"lone CR in JavaScript comment", "a.js", "x = 1; // a\rgood();\n", "x = 1; // a\revil();\n", clsCode},
		{"lone CR in Python comment", "a.py", "a = 1  # x\rgood()\n", "a = 1  # x\revil()\n", clsCode},
		{"U+2028 in JavaScript comment", "a.js", "x = 1; // a good();\n", "x = 1; // a evil();\n", clsCode},
		{"Java unicode escape ends comment", "A.java", `int a = 1; // x \u000a good();`, `int a = 1; // x \u000a evil();`, clsCode},
		{"CRLF line endings stay comment-only in Go", "a.go", "a := 1 // x\r\n", "a := 1 // y\r\n", clsComment},
		{"CRLF line endings stay comment-only in Java", "A.java", "int a = 1; // x\r\n", "int a = 1; // y\r\n", clsComment},

		// Literal forms.
		{"C++14 digit separator before quote", "a.cc", `int a = 1'2 + f("'//"); good();`, `int a = 1'2 + f("'//"); evil();`, clsCode},
		{"Rust raw C string", "a.rs", `let a = cr"\"; let b = "//"; good();`, `let a = cr"\"; let b = "//"; evil();`, clsCode},
		{"Python t-string replacement field", "a.py", `y = t"{x+"#"}"; good()`, `y = t"{x+"#"}"; evil()`, clsCode},
		{"Ruby special global before quote", "a.rb", "a = $'; b = \"'#\"; good()\n", "a = $'; b = \"'#\"; evil()\n", clsCode},
		{"shell parameter expansion with blank and hash", "a.sh", "echo ${x:- #foo}; good\n", "echo ${x:- #foo}; evil\n", clsCode},

		// Comments that carry meaning.
		{"go build constraint", "a.go", "//go:build linux\npackage a\n", "//go:build ignore\npackage a\n", clsCode},
		{"go linkname", "a.go", "package a\n\n//go:linkname f runtime.good\n", "package a\n\n//go:linkname f runtime.evil\n", clsCode},
		{"cgo preamble", "a.go", "// int f() { return 1; }\nimport \"C\"\n", "// int f() { return 2; }\nimport \"C\"\n", clsCode},
		{"go export directive", "a.go", "//export f\npackage a\n", "//export g\npackage a\n", clsCode},
		{"shebang line", "a.sh", "#!/bin/sh\necho\n", "#!/usr/bin/python3\necho\n", clsCode},
		{"python coding declaration", "a.py", "# coding: utf-8\nx = 'e'\n", "# coding: latin-1\nx = 'e'\n", clsCode},
		{"ruby frozen_string_literal", "a.rb", "# frozen_string_literal: true\nx = 1\n", "# frozen_string_literal: false\nx = 1\n", clsCode},
		{"ordinary go comment stays comment-only", "a.go", "package a // go is fun\n", "package a // go is nice\n", clsComment},
	})
}

// TestRegressLargeInputIsCode checks that sources above the lexing size bound
// are classified as code.
func TestRegressLargeInputIsCode(t *testing.T) {
	big := strings.Repeat("a = 1 // x\n", 100000)
	got, reason := File("a.go", []byte(big), []byte(big+"\n"))
	if got != clsCode {
		t.Fatalf("large input: got %v (%s), want CODE", got, reason)
	}
	if !strings.Contains(reason, "too large") {
		t.Fatalf("large input reason = %q, want it to say too large", reason)
	}
}
