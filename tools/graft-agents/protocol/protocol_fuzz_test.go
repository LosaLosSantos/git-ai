package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"
)

// FuzzRequestDecode fuzzes the decoding of the graft-resolve request. Any
// request that decodes must encode again and decode to the same files, sides
// and contents, and the mode and kind checks must not panic.
func FuzzRequestDecode(f *testing.F) {
	for _, seed := range []string{
		`{"version":1,"repository":{"storage":"default","relative_path":"x.git"},` +
			`"branch":"main","tip":"t","commit":"c","merge_base":"m",` +
			`"messages":{"tip":"tip msg","commit":"commit msg"},"actor":"agent-7",` +
			`"files":[{"path":"a/b.go","kind":"content",` +
			`"base":{"oid":"o1","mode":"100644","content":"aGVsbG8K"},` +
			`"ours":null,"theirs":{"oid":"o3","mode":"100755","content":""}}],` +
			`"limits":{"max_file_bytes":1048576}}`,
		`{"version":1,"files":[{"path":"gone.go","kind":"modify-delete","base":null,"ours":null,"theirs":null}]}`,
		`{"files":[{"path":"x","kind":"add-add","ours":{"oid":"","mode":"120000","content":"L3Rt"}}]}`,
		`{"files":[{"path":"x","kind":"mode","theirs":{"content":"!!"}}]}`,
		`{"files":[{"path":"x","kind":"content","base":{"content":null}}]}`,
		`{"limits":{"max_file_bytes":-1}}`,
		`null`,
		`[]`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var req Request
		if err := json.Unmarshal(data, &req); err != nil {
			return
		}

		encoded, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("encoding a decoded request: %v", err)
		}

		var again Request
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatalf("decoding %s: %v", encoded, err)
		}

		if again.Version != req.Version || again.Actor != req.Actor || again.Limits != req.Limits {
			t.Fatalf("header changed in round trip: %s", encoded)
		}

		if len(again.Files) != len(req.Files) {
			t.Fatalf("file count changed: %d != %d", len(again.Files), len(req.Files))
		}

		for i := range req.Files {
			a, b := req.Files[i], again.Files[i]

			if a.Path != b.Path || a.Kind != b.Kind {
				t.Fatalf("file %d changed in round trip: %+v != %+v", i, b, a)
			}

			for _, sides := range [][2]*Side{{a.Base, b.Base}, {a.Ours, b.Ours}, {a.Theirs, b.Theirs}} {
				if (sides[0] == nil) != (sides[1] == nil) {
					t.Fatalf("file %d side presence changed in round trip: %s", i, encoded)
				}

				if sides[0] == nil {
					continue
				}

				if sides[0].OID != sides[1].OID || sides[0].Mode != sides[1].Mode ||
					!bytes.Equal(sides[0].Content, sides[1].Content) {
					t.Fatalf("file %d side changed in round trip: %s", i, encoded)
				}
			}

			_ = ValidKind(a.Kind)
			if a.Theirs != nil {
				_ = ValidMode(a.Theirs.Mode)
			}
		}
	})
}

// FuzzResponseRoundTrip fuzzes the encoding of resolver answers built from
// arbitrary contents. Content must survive the round trip, and a delete must
// carry a null content.
func FuzzResponseRoundTrip(f *testing.F) {
	f.Add("graft-resolve/trivial", "gone.go", "keep.go", []byte("x"), "100644", "")
	f.Add("", "", "", []byte(nil), "", "explained")
	f.Add("r\x00", "a\xff", "b", []byte{0, 1, 2, 255}, "120000", "e")

	f.Fuzz(func(t *testing.T, resolver, deletePath, writePath string, content []byte, mode, explanation string) {
		resp := Response{
			Version:  Version,
			Resolver: resolver,
			Files: []ResolvedFile{
				{Path: deletePath, Action: ActionDelete},
				{Path: writePath, Action: ActionWrite, Mode: mode, Content: content},
			},
			Explanation: explanation,
		}

		encoded, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}

		var decoded Response
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("decoding %s: %v", encoded, err)
		}

		if decoded.Files[0].Content != nil {
			t.Fatalf("delete carries content in %s", encoded)
		}

		if !bytes.Equal(decoded.Files[1].Content, content) {
			t.Fatalf("content changed in round trip: %q != %q", decoded.Files[1].Content, content)
		}

		// encoding/json replaces invalid UTF-8 in strings, so only valid strings
		// survive byte for byte.
		if utf8.ValidString(mode) && decoded.Files[1].Mode != mode {
			t.Fatalf("mode changed in round trip: %q != %q", decoded.Files[1].Mode, mode)
		}

		if utf8.ValidString(explanation) && decoded.Explanation != explanation {
			t.Fatalf("explanation changed in round trip: %q != %q", decoded.Explanation, explanation)
		}
	})
}
