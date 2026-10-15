package classify

import (
	"reflect"
	"testing"
)

func TestTokenizeStreams(t *testing.T) {
	for _, tc := range []struct {
		name string
		lang language
		src  string
		want []string
	}{
		{"go drops comments and keeps string", langGo, "x := \"a//b\" // c\n", []string{"x", ":=", `"a//b"`}},
		{"go raw string spans lines", langGo, "x := `a\nb`\n", []string{"x", ":=", "`a\nb`"}},
		{"go line break is significant", langGo, "a\nb", []string{"a", "\n", "b"}},
		{"go blank lines collapse", langGo, "a\n\n\n// c\nb\n", []string{"a", "\n", "b"}},
		{"java block comment is blank", langJava, "a /* x */ b", []string{"a", "b"}},
		{"java line break is blank", langJava, "a\nb", []string{"a", "b"}},
		{"java javadoc is a token", langJava, "/** d */ a", []string{"/** d */", "a"}},
		{"java text block is a token", langJava, "s = \"\"\"\n  x\n  \"\"\";", []string{"s", "=", "\"\"\"\n  x\n  \"\"\"", ";"}},
		{"rust nested comment", langRust, "a /* x /* y */ z */ b", []string{"a", "b"}},
		{"rust byte string joins prefix", langRust, `b"x"`, []string{`b"x"`}},
		{"rust raw identifier joins prefix", langRust, "r#type", []string{"r#type"}},
		{"python indentation kept", langPython, "if x:\n    y\n", []string{"\x01", "if", "x", ":", "\n", "\x01    ", "y"}},
		{"python comment-only line ignored", langPython, "x\n    # c\ny\n", []string{"\x01", "x", "\n", "\x01", "y"}},
		{"python continuation is a token", langPython, "x \\\n  y", []string{"\x01", "x", "\\\n", "y"}},
		{"python prefix joins string", langPython, "rb'x'", []string{"\x01", "rb'x'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, ok := tokenize(tc.lang, []byte(tc.src))
			if !ok {
				t.Fatalf("tokenize not certain: %s", reason)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tokens = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTokenizeUncertain(t *testing.T) {
	for _, tc := range []struct {
		name string
		lang language
		src  string
	}{
		{"unterminated go string", langGo, "x := \"a\n"},
		{"unterminated block comment", langJava, "a /* b"},
		{"unterminated rust raw string", langRust, `r#"x"`},
		{"unterminated python triple string", langPython, "x = '''a"},
		{"unterminated java text block", langJava, "s = \"\"\"\nx"},
		{"java text block without line break", langJava, `s = """x""";`},
		{"go cgo line", langGo, "// #cgo CFLAGS: -DX\n"},
		{"go invalid UTF-8", langGo, "x := \"\xff\"\n"},
		{"rust shebang", langRust, "#!/bin/run\nfn f() {}\n"},
		{"rust stringify", langRust, "stringify!(a)"},
		{"rust string suffix", langRust, `"x"s`},
		{"python backslash before blank", langPython, "x \\ \ny"},
		{"python name before quote", langPython, "ab'x'"},
		{"unknown language", langUnknown, "a"},
		{"binary", langGo, "a\x00b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := tokenize(tc.lang, []byte(tc.src)); ok {
				t.Fatal("tokenize must be uncertain")
			}
		})
	}
}

func TestTokenizeWhitespaceIsNotSignificantInJava(t *testing.T) {
	a, _, okA := tokenize(langJava, []byte("x = -1;"))
	b, _, okB := tokenize(langJava, []byte("x=-1 ;"))
	if !okA || !okB || !equalTokens(a, b) {
		t.Fatalf("expected equal streams, got %q and %q", a, b)
	}
}

func TestLanguageOfOnlySupportsFourLanguages(t *testing.T) {
	for path, want := range map[string]language{
		"a.go": langGo, "A.java": langJava, "a.rs": langRust, "a.py": langPython,
		"a.c": langUnknown, "a.h": langUnknown, "a.cc": langUnknown, "a.cpp": langUnknown,
		"a.hpp": langUnknown, "a.js": langUnknown, "a.mjs": langUnknown, "a.ts": langUnknown,
		"a.jsx": langUnknown, "a.tsx": langUnknown, "a.sh": langUnknown, "a.bash": langUnknown,
		"a.rb": langUnknown, "Makefile": langUnknown,
	} {
		if got := languageOf(path); got != want {
			t.Errorf("languageOf(%q) = %d, want %d", path, got, want)
		}
	}
}
