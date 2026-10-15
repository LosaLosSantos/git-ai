package resolver

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// FuzzValidate fuzzes the acceptance rules of the resolver answers. Whatever
// the request and the response, Validate must not panic, and an accepted answer
// must answer every conflicted path exactly once, with valid modes and within
// the size limit.
func FuzzValidate(f *testing.F) {
	request := `{"version":1,"repository":{"storage":"default","relative_path":"x.git"},` +
		`"branch":"main","tip":"t","commit":"c","merge_base":"m","messages":{"tip":"a","commit":"b"},` +
		`"actor":"agent-7","files":[{"path":"a/b.go","kind":"content","base":null,` +
		`"ours":{"oid":"o1","mode":"100644","content":"aGVsbG8K"},"theirs":null},` +
		`{"path":"c.txt","kind":"add-add","base":null,"ours":null,"theirs":null}],` +
		`"limits":{"max_file_bytes":1048576}}`

	for _, seed := range []struct{ req, resp string }{
		{request, `{"version":1,"resolver":"trivial","files":[` +
			`{"path":"a/b.go","action":"write","mode":"100644","content":"aGVsbG8K"},` +
			`{"path":"c.txt","action":"delete"}],"explanation":"ok"}`},
		{request, `{"version":1,"resolver":"x","files":[{"path":"a/b.go","action":"write","mode":"100644","content":"PDw8PDw8PDw8IG91cnMKPT09PT09PQo="}]}`},
		{request, `{"version":1,"resolver":"x","files":[{"path":"a/b.go","action":"write","mode":"100664"}]}`},
		{request, `{"version":2,"resolver":"x","files":[]}`},
		{request, `{"version":1,"resolver":"x","files":[{"path":"other","action":"delete"}]}`},
		{request, `{"version":1,"resolver":"x","files":[{"path":"c.txt","action":"write","mode":"120000","content":"eA=="}]}`},
		{`{"limits":{"max_file_bytes":0},"files":[]}`, `{"version":1,"files":[]}`},
		{`null`, `null`},
	} {
		f.Add([]byte(seed.req), []byte(seed.resp))
	}

	f.Fuzz(func(t *testing.T, requestData, responseData []byte) {
		var req Request
		if err := json.Unmarshal(requestData, &req); err != nil {
			return
		}

		var resp Response
		if err := json.Unmarshal(responseData, &resp); err != nil {
			return
		}

		if err := Validate(&req, &resp); err != nil {
			return
		}

		// An accepted answer must be the exact set of the conflicted paths.
		conflicted := make(map[string]struct{}, len(req.Files))
		for _, f := range req.Files {
			conflicted[f.Path] = struct{}{}
		}

		answered := make(map[string]struct{}, len(resp.Files))
		for _, f := range resp.Files {
			if _, ok := conflicted[f.Path]; !ok {
				t.Fatalf("accepted answer for path %q that is not conflicted", f.Path)
			}

			if _, dup := answered[f.Path]; dup {
				t.Fatalf("accepted answer that repeats path %q", f.Path)
			}
			answered[f.Path] = struct{}{}

			if f.Action == ActionWrite {
				if !validMode(f.Mode) {
					t.Fatalf("accepted write with mode %q", f.Mode)
				}

				if int64(len(f.Content)) > req.Limits.MaxFileBytes {
					t.Fatalf("accepted %d bytes over the limit %d", len(f.Content), req.Limits.MaxFileBytes)
				}
			}
		}

		if len(answered) != len(conflicted) {
			t.Fatalf("accepted answer covers %d of %d conflicted paths", len(answered), len(conflicted))
		}
	})
}

// FuzzSideJSON fuzzes the base64 wire form of a file side. A decoded side must
// encode and decode to the same OID, mode and content.
func FuzzSideJSON(f *testing.F) {
	for _, seed := range []string{
		`{"oid":"o1","mode":"100644","content":"aGVsbG8K"}`,
		`{"oid":"o1","mode":"100755","content":""}`,
		`{"oid":"","mode":"","content":"/w=="}`,
		`{"oid":"o1","mode":"100644","content":"not base64!"}`,
		`{"oid":"o1","mode":"100644"}`,
		`null`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var side Side
		if err := json.Unmarshal(data, &side); err != nil {
			return
		}

		encoded, err := json.Marshal(side)
		if err != nil {
			t.Fatalf("encoding a decoded side: %v", err)
		}

		var again Side
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatalf("decoding %s: %v", encoded, err)
		}

		if again.OID != side.OID || again.Mode != side.Mode || !bytes.Equal(again.Content, side.Content) {
			t.Fatalf("side changed in round trip: %+v != %+v", again, side)
		}
	})
}

// FuzzAllowed fuzzes the path policy of the resolver. A path that is not clean
// is never allowed, deny wins over allow, and literal globs match exactly.
func FuzzAllowed(f *testing.F) {
	for _, seed := range []struct{ path, allow, deny string }{
		{"a/b.go", "a/**", "a/secret/**"},
		{"secrets/a/b.env", "**", "secrets/**"},
		{"secrets", "secrets/**", ""},
		{"/etc/passwd", "**", ""},
		{"a/../b", "**", ""},
		{"a//b", "", ""},
		{"", "", ""},
		{"x.go", "*.go", "x.go"},
		{"dir/file", "dir**", "dir/file"},
		{"a\x00b", "**", ""},
		{"café/\xff", "café/**", "["},
	} {
		f.Add(seed.path, seed.allow, seed.deny)
	}

	f.Fuzz(func(t *testing.T, p, allowGlob, denyGlob string) {
		clean := cleanRelPath(p)

		if !clean && Allowed(p, nil, nil) {
			t.Fatalf("unclean path %q allowed", p)
		}

		if got := Allowed(p, nil, nil); got != clean {
			t.Fatalf("Allowed(%q, nil, nil) = %v, want %v", p, got, clean)
		}

		if Allowed(p, []string{allowGlob}, []string{denyGlob}) {
			if m, err := matchGlob(denyGlob, p); err == nil && m {
				t.Fatalf("path %q allowed although deny glob %q matches", p, denyGlob)
			}
		}

		if isLiteralGlob(allowGlob) && clean {
			if got, err := matchGlob(allowGlob, p); err != nil || got != (allowGlob == p) {
				t.Fatalf("literal glob %q against %q = %v, %v", allowGlob, p, got, err)
			}
		}

		if prefix, ok := strings.CutSuffix(allowGlob, "/**"); ok && isLiteralGlob(prefix) {
			got, err := matchGlob(allowGlob, p)
			if err != nil || got != strings.HasPrefix(p, prefix+"/") {
				t.Fatalf("deep glob %q against %q = %v, %v", allowGlob, p, got, err)
			}
		}

		if loose, ok := strings.CutSuffix(allowGlob, "**"); ok && allowGlob != "**" && !strings.HasSuffix(allowGlob, "/**") && isLiteralGlob(loose) {
			got, err := matchGlob(allowGlob, p)
			if err != nil || got != strings.HasPrefix(p, loose) {
				t.Fatalf("loose glob %q against %q = %v, %v", allowGlob, p, got, err)
			}
		}

		// ActorAllowed: an empty actor never matches a non-empty list, and a
		// literal glob matches only the identical actor.
		if ActorAllowed(p, []string{allowGlob}) && p == "" {
			t.Fatalf("empty actor allowed by %q", allowGlob)
		}

		if p != "" && isLiteralGlob(allowGlob) && ActorAllowed(p, []string{allowGlob}) != (allowGlob == p) {
			t.Fatalf("ActorAllowed(%q, [%q]) inconsistent with literal match", p, allowGlob)
		}
	})
}

// isLiteralGlob reports whether glob has no path.Match metacharacter, so that it
// matches only itself.
func isLiteralGlob(glob string) bool {
	return !strings.ContainsAny(glob, `*?[]\`)
}
