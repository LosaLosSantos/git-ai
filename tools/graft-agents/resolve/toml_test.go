package resolve

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseTOMLValues(t *testing.T) {
	src := `# comment
top = "a\"b\n\u00e9\U0001F600"
lit = 'C:\raw\path'
n = 42
neg = -7
big = 1_000
f = 2.5
t = true
f2 = false
list = ["x", 'y', "z",]   # trailing comma and comment
multi = [
  "make", # first
  "-f",
  "{out}",
]
"quoted key" = 1
`
	got, err := parseTOML(src)
	if err != nil {
		t.Fatalf("parseTOML: %v", err)
	}
	want := map[string]any{
		"top":        "a\"b\né😀",
		"lit":        `C:\raw\path`,
		"n":          int64(42),
		"neg":        int64(-7),
		"big":        int64(1000),
		"f":          2.5,
		"t":          true,
		"f2":         false,
		"list":       []any{"x", "y", "z"},
		"multi":      []any{"make", "-f", "{out}"},
		"quoted key": int64(1),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseTOML =\n%#v\nwant\n%#v", got, want)
	}
}

func TestParseTOMLTables(t *testing.T) {
	src := `
[[strategy]]
type = "trivial"

[[strategy]]
type = "union"
paths = ["CHANGELOG.md"]

[strategy.llm]
model = "m1"
effort = "high"

[[strategy]]
type = "rerere"
dir = "/x"

[settings]
timeout = "1m"
`
	got, err := parseTOML(src)
	if err != nil {
		t.Fatalf("parseTOML: %v", err)
	}
	strategies, ok := got["strategy"].([]map[string]any)
	if !ok || len(strategies) != 3 {
		t.Fatalf("strategy = %#v", got["strategy"])
	}
	if strategies[0]["type"] != "trivial" || strategies[2]["dir"] != "/x" {
		t.Fatalf("elements wrong: %#v", strategies)
	}
	llm, ok := strategies[1]["llm"].(map[string]any)
	if !ok || llm["model"] != "m1" {
		t.Fatalf("dotted header must attach to the last element: %#v", strategies[1])
	}
	if _, ok := strategies[2]["llm"]; ok {
		t.Fatal("sub-table leaked into a later element")
	}
	if got["settings"].(map[string]any)["timeout"] != "1m" {
		t.Fatalf("settings = %#v", got["settings"])
	}
}

func TestParseTOMLErrors(t *testing.T) {
	cases := map[string]string{
		"unterminated string":    `a = "abc`,
		"newline in string":      "a = \"ab\ncd\"",
		"bad escape":             `a = "\q"`,
		"inline table":           `a = {b = 1}`,
		"duplicate key":          "a = 1\na = 2",
		"duplicate table":        "[t]\n[t]",
		"missing equals":         "a 1",
		"unterminated array":     `a = ["x"`,
		"unknown value":          `a = 1979-05-27`,
		"array of table clash":   "a = 1\n[[a]]",
		"trailing garbage":       `a = 1 b`,
		"empty value":            `a = `,
		"bad header close":       "[a",
		"unterminated literal":   `a = 'abc`,
		"dotted assignment":      `a.b = 1`,
		"bad unicode escape":     `a = "\uZZZZ"`,
		"short unicode escape":   `a = "\u12"`,
		"surrogate unicode":      `a = "\uD800"`,
		"missing comma in array": `a = ["x" "y"]`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTOML(src); err == nil {
				t.Fatalf("parseTOML(%q) accepted invalid input", src)
			}
		})
	}
}

func TestParseTOMLErrorsReportLine(t *testing.T) {
	_, err := parseTOML("a = 1\n\nb = \"open\n")
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("error = %v, want line 3", err)
	}
}
