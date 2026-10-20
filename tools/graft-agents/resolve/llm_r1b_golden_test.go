package resolve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The guarantee that the step of the design (PLAN10 15.10) rests on: the first request of a region is that of
// R1b, byte for byte (the options that could change it are gone). The golden files in testdata/r1b were
// written by the code of R1b (the tree before the design step, GRAFT_WRITE_R1B_GOLDEN=1), for the files
// below, and this test compares what the strategy sends now with them. The nonce of a request is replaced by NONCE.
//
// The files are chosen so that git's merge and our own line diff agree on their regions: the regions
// are another matter (llmFileRegions), the request of a region is what is pinned here.

func r1bGoldenFiles() map[string]LLMFile {
	base := numbered("line", 30)
	two := func(s string) string {
		return strings.NewReplacer("line 5\n", s+"5\n", "line 20\n", s+"20\n").Replace(base)
	}
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	long := strings.Repeat("é", llmMaxMessageBytes)
	return map[string]LLMFile{
		"single-with-message": {Path: "a.txt", Base: []byte(base), Ours: []byte(strings.Replace(base, "line 5\n", "o5\n", 1)),
			Theirs: []byte(strings.Replace(base, "line 5\n", "t5\n", 1)), TheirsMessage: "theirs did that\n\nwith a body"},
		"single-no-message": {Path: "a.txt", Base: []byte(base), Ours: []byte(strings.Replace(base, "line 5\n", "o5\n", 1)),
			Theirs: []byte(strings.Replace(base, "line 5\n", "t5\n", 1))},
		"two-regions": {Path: "two.go", Base: []byte(base), Ours: []byte(two("o")), Theirs: []byte(two("t")), TheirsMessage: "both lines"},
		"first-line":  {Path: "f.txt", Base: []byte(base), Ours: []byte(strings.Replace(base, "line 1\n", "o1\n", 1)), Theirs: []byte(strings.Replace(base, "line 1\n", "t1\n", 1))},
		"last-line":   {Path: "l.txt", Base: []byte(base), Ours: []byte(strings.Replace(base, "line 30\n", "o30\n", 1)), Theirs: []byte(strings.Replace(base, "line 30\n", "t30\n", 1))},
		"crlf": {Path: "c.txt", Base: []byte(crlf(base)), Ours: []byte(crlf(strings.Replace(base, "line 5\n", "o5\n", 1))),
			Theirs: []byte(crlf(strings.Replace(base, "line 5\n", "t5\n", 1))), TheirsMessage: "crlf"},
		"cut-message": {Path: "m.txt", Base: []byte(base), Ours: []byte(strings.Replace(base, "line 5\n", "o5\n", 1)),
			Theirs: []byte(strings.Replace(base, "line 5\n", "t5\n", 1)), TheirsMessage: long},
		"no-final-newline":  {Path: "n.txt", Base: []byte("a\nb\nc"), Ours: []byte("a\nX\nc"), Theirs: []byte("a\nY\nc"), TheirsMessage: "no newline"},
		"marker-like-lines": {Path: "k.txt", Base: []byte("x\n=======\nm\n"), Ours: []byte("x\n=======\nours\n"), Theirs: []byte("x\n=======\ntheirs\n")},
	}
}

func TestR1bRequestsAreByteForByte(t *testing.T) {
	files := r1bGoldenFiles()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			p := llmFixed("merged\n")
			if _, err := ResolveLLM(context.Background(), p, files[name], LLMOptions{}); err != nil {
				t.Fatal(err)
			}
			var sb strings.Builder
			for i, req := range p.reqs {
				m := patchNonceRE.FindStringSubmatch(req.Prompt)
				if m == nil {
					t.Fatalf("no nonce in request %d", i)
				}
				fmt.Fprintf(&sb, "=== request %d: system (%d bytes)\n%s\n=== request %d: prompt (%d bytes)\n%s\n",
					i+1, len(req.System), strings.ReplaceAll(req.System, m[1], "NONCE"),
					i+1, len(req.Prompt), strings.ReplaceAll(req.Prompt, m[1], "NONCE"))
			}
			path := filepath.Join("testdata", "r1b", name+".golden")
			if os.Getenv("GRAFT_WRITE_R1B_GOLDEN") != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if sb.String() != string(want) {
				t.Fatalf("the requests of R1b changed for %s:\n%s", name, firstDifference(string(want), sb.String()))
			}
		})
	}
}

// firstDifference words where two texts first differ.
func firstDifference(want, got string) string {
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	cut := func(s string) string { return s[max(0, i-40):min(len(s), i+80)] }
	return fmt.Sprintf("at byte %d (want %d bytes, got %d)\nwant ...%q\ngot  ...%q", i, len(want), len(got), cut(want), cut(got))
}
