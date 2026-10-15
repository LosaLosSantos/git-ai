package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestWireFormat(t *testing.T) {
	in := `{"version":1,
	"repository":{"storage":"default","relative_path":"x.git"},
	"branch":"main","tip":"t","commit":"c","merge_base":"m",
	"messages":{"tip":"tip msg","commit":"commit msg"},
	"actor":"agent-7",
	"files":[{"path":"a/b.go","kind":"content",
	  "base":{"oid":"o1","mode":"100644","content":"aGVsbG8K"},
	  "ours":null,
	  "theirs":{"oid":"o3","mode":"100755","content":""}}],
	"limits":{"max_file_bytes":1048576}}`

	var req Request
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Version != Version || req.Repository.RelativePath != "x.git" || req.Actor != "agent-7" {
		t.Fatalf("header fields wrong: %+v", req)
	}
	if req.Limits.MaxFileBytes != 1048576 {
		t.Fatalf("limits = %+v", req.Limits)
	}
	f := req.Files[0]
	if f.Base == nil || string(f.Base.Content) != "hello\n" {
		t.Fatalf("base content not decoded from base64: %+v", f.Base)
	}
	if f.Ours != nil {
		t.Fatalf("null ours must decode to nil, got %+v", f.Ours)
	}
	if f.Theirs == nil || f.Theirs.Content == nil || len(f.Theirs.Content) != 0 || f.Theirs.Mode != ModeExecutable {
		t.Fatalf("theirs = %+v", f.Theirs)
	}

	out, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var again Request
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if string(again.Files[0].Base.Content) != "hello\n" || again.Files[0].Ours != nil {
		t.Fatalf("round trip changed the request: %s", out)
	}
}

func TestResponseDeleteEncoding(t *testing.T) {
	resp := Response{
		Version:  Version,
		Resolver: "graft-resolve/trivial",
		Files: []ResolvedFile{
			{Path: "gone.go", Action: ActionDelete},
			{Path: "keep.go", Action: ActionWrite, Mode: ModeFile, Content: []byte("x")},
		},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `{"path":"gone.go","action":"delete","content":null}`) {
		t.Fatalf("delete encoding: %s", s)
	}
	if !strings.Contains(s, `{"path":"keep.go","action":"write","mode":"100644","content":"eA=="}`) {
		t.Fatalf("write encoding: %s", s)
	}
	if strings.Contains(s, "explanation") {
		t.Fatalf("empty explanation must be omitted: %s", s)
	}
}

func TestValidModeAndKind(t *testing.T) {
	for _, m := range []string{ModeFile, ModeExecutable, ModeSymlink} {
		if !ValidMode(m) {
			t.Errorf("ValidMode(%q) = false", m)
		}
	}
	for _, m := range []string{"", "100664", "040000", "100644 "} {
		if ValidMode(m) {
			t.Errorf("ValidMode(%q) = true", m)
		}
	}
	for _, k := range []string{KindContent, KindModifyDelete, KindAddAdd, KindMode} {
		if !ValidKind(k) {
			t.Errorf("ValidKind(%q) = false", k)
		}
	}
	if ValidKind("rename") || ValidKind("") {
		t.Error("unknown kinds accepted")
	}
}
