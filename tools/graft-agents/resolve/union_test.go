package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

func TestUnionMerge(t *testing.T) {
	cases := []struct {
		name, base, ours, theirs, want string
	}{
		{
			name:   "both append at end (changelog)",
			base:   "## 1.0\n- a\n",
			ours:   "## 1.0\n- a\n- ours\n",
			theirs: "## 1.0\n- a\n- theirs\n",
			want:   "## 1.0\n- a\n- ours\n- theirs\n",
		},
		{
			name:   "non overlapping edits merge cleanly",
			base:   "1\n2\n3\n4\n5\n",
			ours:   "one\n2\n3\n4\n5\n",
			theirs: "1\n2\n3\n4\nfive\n",
			want:   "one\n2\n3\n4\nfive\n",
		},
		{
			name:   "touching edits merge cleanly",
			base:   "1\n2\n3\n",
			ours:   "ONE\n2\n3\n",
			theirs: "1\n2\nTHREE\n",
			want:   "ONE\n2\nTHREE\n",
		},
		{
			name:   "same region changed differently keeps both",
			base:   "x\nshared\ny\n",
			ours:   "x\nours\ny\n",
			theirs: "x\ntheirs\ny\n",
			want:   "x\nours\ntheirs\ny\n",
		},
		{
			name:   "identical change on both sides appears once",
			base:   "a\n",
			ours:   "a\nnew\n",
			theirs: "a\nnew\n",
			want:   "a\nnew\n",
		},
		{
			name:   "only ours changed",
			base:   "a\nb\n",
			ours:   "a\nB\n",
			theirs: "a\nb\n",
			want:   "a\nB\n",
		},
		{
			name:   "only theirs changed",
			base:   "a\nb\n",
			ours:   "a\nb\n",
			theirs: "a\nB\n",
			want:   "a\nB\n",
		},
		{
			name:   "add-add both sides keep both",
			base:   "",
			ours:   "ours line\n",
			theirs: "theirs line\n",
			want:   "ours line\ntheirs line\n",
		},
		{
			name:   "ours lacks trailing newline",
			base:   "a\n",
			ours:   "a\nours",
			theirs: "a\ntheirs\n",
			want:   "a\nours\ntheirs\n",
		},
		{
			name:   "deleted line on one side",
			base:   "a\nb\nc\n",
			ours:   "a\nc\n",
			theirs: "a\nb\nc\nd\n",
			want:   "a\nc\nd\n",
		},
		{
			name:   "both identical to base",
			base:   "same\n",
			ours:   "same\n",
			theirs: "same\n",
			want:   "same\n",
		},
		{
			name:   "empty everything",
			base:   "",
			ours:   "",
			theirs: "",
			want:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := unionMerge([]byte(c.base), []byte(c.ours), []byte(c.theirs))
			if err != nil {
				t.Fatalf("unionMerge: %v", err)
			}
			if string(got) != c.want {
				t.Fatalf("unionMerge =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}

func TestUnionMergeLargeFileBounded(t *testing.T) {
	big := strings.Repeat("line\n", 3000) // 3000 lines on each side
	other := strings.Repeat("other\n", 3000)
	if _, err := unionMerge([]byte(big), []byte(other), []byte(big+"x\n")); err == nil {
		t.Fatal("oversized LCS accepted")
	}
}

func TestUnionStrategy(t *testing.T) {
	base := sideOf(protocol.ModeFile, "a\n")
	ours := sideOf(protocol.ModeFile, "a\nours\n")
	theirs := sideOf(protocol.ModeFile, "a\ntheirs\n")

	s, err := newUnion(StrategySpec{Name: "u", Type: "union", Paths: []string{"CHANGELOG.md"}})
	if err != nil {
		t.Fatal(err)
	}

	res, ok, err := s.Resolve(context.Background(), File{Base: base, Ours: ours, Theirs: theirs}, protocol.Request{})
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if res.Action != protocol.ActionWrite || res.Mode != protocol.ModeFile || string(res.Content) != "a\nours\ntheirs\n" {
		t.Fatalf("answer = %+v", res)
	}

	declined := map[string]File{
		"modify-delete (theirs absent)": {Base: base, Ours: ours, Theirs: nil},
		"ours absent":                   {Base: base, Ours: nil, Theirs: theirs},
		"symlink ours":                  {Base: base, Ours: sideOf(protocol.ModeSymlink, "x"), Theirs: theirs},
		"mode mismatch":                 {Base: base, Ours: ours, Theirs: sideOf(protocol.ModeExecutable, "a\ntheirs\n")},
		"binary ours":                   {Base: base, Ours: sideOf(protocol.ModeFile, "\x00a"), Theirs: theirs},
		"binary base":                   {Base: sideOf(protocol.ModeFile, "\x00"), Ours: ours, Theirs: theirs},
	}
	for name, f := range declined {
		t.Run(name, func(t *testing.T) {
			_, ok, err := s.Resolve(context.Background(), f, protocol.Request{})
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if ok {
				t.Fatal("answered a file it must decline")
			}
		})
	}
}

func TestUnionFactoryRequiresPaths(t *testing.T) {
	if _, err := newUnion(StrategySpec{Name: "u", Type: "union"}); err == nil {
		t.Fatal("union without paths accepted")
	}
	if _, err := newUnion(StrategySpec{Name: "u", Type: "union", Paths: []string{"x"}, Options: map[string]any{"y": 1}}); err == nil {
		t.Fatal("unknown option accepted")
	}
}

func TestSplitLines(t *testing.T) {
	cases := map[string][]string{
		"":         nil,
		"a":        {"a"},
		"a\n":      {"a\n"},
		"a\nb":     {"a\n", "b"},
		"a\n\nb\n": {"a\n", "\n", "b\n"},
	}
	for in, want := range cases {
		got := splitLines([]byte(in))
		if strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("splitLines(%q) = %q, want %q", in, got, want)
		}
	}
}
