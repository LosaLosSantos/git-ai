package syntax

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckEveryGrammar(t *testing.T) {
	for _, tc := range []struct {
		path, valid, invalid string
	}{
		{"a.ts", "const x: number = 1;\nexport function f(a: string): string { return a; }\n", "const x: number = ;\n"},
		{"a.tsx", "export const A = () => <div className=\"a\">{1}</div>;\n", "export const A = (): JSX.Element => <div>{1</div>;\n"},
		{"a.js", "const a = [1, 2].map((x) => x * 2);\n", "const a = [1, 2.map((x) => x * 2);\n"},
		{"a.jsx", "const A = () => <p>hi</p>;\n", "const A = () => <p>hi</p;\n"},
		{"a.py", "def f(x):\n    return x + 1\n", "def f(x)\n    return x + 1\n"},
		{"a.go", "package a\n\nfunc F(x int) int { return x + 1 }\n", "package a\n\nfunc F(x int) int { return x + }\n"},
		{"a.rs", "fn f(x: i32) -> i32 { x + 1 }\n", "fn f(x: i32) -> i32 { x + }\n"},
		{"A.java", "class A { int f(int x) { return x + 1; } }\n", "class A { int f(int x) { return x + 1 } }\n"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if err := Check(context.Background(), tc.path, []byte(tc.valid)); err != nil {
				t.Fatalf("valid content rejected: %v", err)
			}
			err := Check(context.Background(), tc.path, []byte(tc.invalid))
			var se *Error
			if !errors.As(err, &se) {
				t.Fatalf("invalid content: err = %v, want a syntax error", err)
			}
			if se.Path != tc.path || se.Line < 1 || se.Column < 1 {
				t.Fatalf("error %+v", se)
			}
		})
	}
}

// TestCheckGrammarLimits records what the grammars accept although a compiler would not: the
// check is a parse, not a build. A JSX closing tag that does not match its opening tag parses.
func TestCheckGrammarLimits(t *testing.T) {
	for p, src := range map[string]string{
		"a.tsx":  "export const A = () => <div>{1}</span>;\n",
		"a.go":   "package a\n\nfunc F() int { return undefinedName }\n",
		"A.java": "class A { void f() { int x = \"not an int\"; } }\n",
	} {
		if err := Check(context.Background(), p, []byte(src)); err != nil {
			t.Errorf("%s: %v (if the grammar now rejects this, update the documented limits)", p, err)
		}
	}
}

func TestCheckReportsFirstErrorPosition(t *testing.T) {
	src := "package a\n\nfunc F() {}\n\nfunc G() { x := }\n\nfunc H() { y := }\n"
	err := Check(context.Background(), "a.go", []byte(src))
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("err = %v", err)
	}
	if se.Line != 5 || se.Language != Go {
		t.Fatalf("error %+v, want the first one, on line 5", se)
	}
	if !strings.Contains(se.Error(), "a.go does not parse as go") || !strings.Contains(se.Error(), "line 5") {
		t.Fatalf("message %q", se.Error())
	}
}

func TestCheckReportsMissingNode(t *testing.T) {
	err := Check(context.Background(), "A.java", []byte("class A { int f() { return 1 } }\n"))
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("err = %v", err)
	}
	if se.Missing != ";" || !strings.Contains(se.Error(), `missing ";"`) {
		t.Fatalf("error %+v (%v), want a missing \";\"", se, se)
	}
}

func TestCheckLeavesOtherLanguagesAlone(t *testing.T) {
	for _, p := range []string{"README.md", "a.c", "Makefile", "a.json", "a.yaml", "go.mod", "a.min.css"} {
		if _, ok := LanguageOf(p); ok {
			t.Fatalf("%s has a grammar", p)
		}
		if err := Check(context.Background(), p, []byte("{{{ not code")); err != nil {
			t.Fatalf("%s checked: %v", p, err)
		}
	}
}

func TestLanguageOf(t *testing.T) {
	for p, want := range map[string]string{
		"src/a.TS": TypeScript, "a.d.ts": TypeScript, "a.mts": TypeScript, "a.cts": TypeScript, "a.tsx": TSX,
		"a.js": JavaScript, "a.mjs": JavaScript, "a.cjs": JavaScript, "a.jsx": JavaScript,
		"a.py": Python, "a.pyi": Python, "a.go": Go, "lib.rs": Rust, "A.java": Java,
	} {
		if got, ok := LanguageOf(p); !ok || got != want {
			t.Errorf("LanguageOf(%q) = %q, %v; want %q", p, got, ok, want)
		}
	}
}

func TestCheckEmptyContent(t *testing.T) {
	// An empty Python or JavaScript file is a valid program.
	for _, p := range []string{"a.py", "a.js", "a.ts"} {
		if err := Check(context.Background(), p, nil); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

func TestCheckHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	big := strings.Repeat("x = [1, 2, 3]\n", 200000)
	if err := Check(ctx, "a.py", []byte(big)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
