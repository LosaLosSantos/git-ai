//go:build cgo

package resolve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

func checkSide(content string) *protocol.Side {
	return &protocol.Side{OID: "0123", Mode: protocol.ModeFile, Content: []byte(content)}
}

func checkRequest(resolution ...protocol.ResolvedFile) protocol.Request {
	files := []protocol.FileConflict{
		{Path: "a.go", Kind: protocol.KindContent, Base: checkSide("package a\n\nvar x = 1\n"), Ours: checkSide("package a\n\nvar x = 2\n"), Theirs: checkSide("package a\n\nvar x = 3\n")},
		{Path: "b.js", Kind: protocol.KindContent, Base: checkSide("// @flow\nconst x: ?number = 1;\n"), Ours: checkSide("// @flow\nconst x: ?number = 2;\n"), Theirs: checkSide("// @flow\nconst x: ?number = 3;\n")},
		{Path: "c.txt", Kind: protocol.KindContent, Base: checkSide("1\n"), Ours: checkSide("2\n"), Theirs: checkSide("3\n")},
	}
	return protocol.Request{
		Version: protocol.Version, Operation: protocol.OperationCheck,
		Repository: protocol.Repository{Storage: "default", RelativePath: "x.git"},
		Branch:     "main", Tip: "tip", Commit: "commit", Actor: "agent-1",
		Files: files, Resolution: resolution,
		Limits: protocol.Limits{MaxFileBytes: 1 << 20},
	}
}

func write(p, content string) protocol.ResolvedFile {
	return protocol.ResolvedFile{Path: p, Action: protocol.ActionWrite, Mode: protocol.ModeFile, Content: []byte(content)}
}

func newCheckResolver(t *testing.T) *Resolver {
	t.Helper()
	cfg, err := ParseConfig([]byte("[[strategy]]\ntype = \"trivial\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCheckGivesAVerdictPerFile(t *testing.T) {
	r := newCheckResolver(t)
	resp, err := r.Check(context.Background(), checkRequest(
		write("a.go", "package a\n\nvar x = 2 + 3\n"),
		write("b.js", "// @flow\nconst x: ?number = 5;\n"),
		protocol.ResolvedFile{Path: "c.txt", Action: protocol.ActionDelete},
	))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Operation != protocol.OperationCheck || resp.Version != protocol.Version || len(resp.Files) != 3 {
		t.Fatalf("response %+v", resp)
	}
	want := []protocol.CheckFile{
		{Path: "a.go", OK: true, Syntax: protocol.SyntaxChecked},
		{Path: "b.js", OK: true, Syntax: protocol.SyntaxUnchecked}, // Flow: the sides do not parse
		{Path: "c.txt", OK: true, Syntax: protocol.SyntaxNone},
	}
	for i, w := range want {
		if resp.Files[i] != w {
			t.Errorf("file %d: %+v, want %+v", i, resp.Files[i], w)
		}
	}
}

func TestCheckRejectsWhatAnLLMAnswerWouldFail(t *testing.T) {
	r := newCheckResolver(t)
	for name, tc := range map[string]struct {
		file protocol.ResolvedFile
		want string
	}{
		"syntax":  {write("a.go", "package a\n\nvar x = \n"), "does not parse as go"},
		"markers": {write("a.go", "package a\n<<<<<<< ours\nvar x = 2\n=======\nvar x = 3\n>>>>>>> theirs\n"), "conflict markers"},
		"mode":    {protocol.ResolvedFile{Path: "a.go", Action: protocol.ActionWrite, Mode: "160000", Content: []byte("x")}, "invalid mode"},
		"action":  {protocol.ResolvedFile{Path: "a.go", Action: "rename"}, "unknown action"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := r.Check(context.Background(), checkRequest(tc.file, write("b.js", "x\n"), write("c.txt", "x\n")))
			if err != nil {
				t.Fatal(err)
			}
			if f := resp.Files[0]; f.OK || !strings.Contains(f.Reason, tc.want) {
				t.Fatalf("verdict %+v, want a rejection mentioning %q", f, tc.want)
			}
		})
	}
}

func TestCheckRefusesMalformedRequests(t *testing.T) {
	r := newCheckResolver(t)
	ok := []protocol.ResolvedFile{write("a.go", "package a\n"), write("b.js", "x\n"), write("c.txt", "x\n")}
	for name, req := range map[string]protocol.Request{
		"missing file":  checkRequest(ok[:2]...),
		"unknown path":  checkRequest(ok[0], ok[1], write("d.txt", "x\n")),
		"named twice":   checkRequest(ok[0], ok[0], ok[2]),
		"not a check":   func() protocol.Request { q := checkRequest(ok...); q.Operation = "resolve"; return q }(),
		"wrong version": func() protocol.Request { q := checkRequest(ok...); q.Version = 9; return q }(),
	} {
		if _, err := r.Check(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	// A resolve request must not carry a check operation or a resolution.
	q := checkRequest(ok...)
	if _, err := r.Resolve(context.Background(), q); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("resolve of a check request: err = %v", err)
	}
	q.Operation = ""
	if _, err := r.Resolve(context.Background(), q); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("resolve with a resolution: err = %v", err)
	}
}
