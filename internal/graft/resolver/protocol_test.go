package resolver

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestWireFormat(t *testing.T) {
	t.Parallel()

	req := Request{
		Version:    ProtocolVersion,
		Repository: Repository{Storage: "default", RelativePath: "x.git"},
		Branch:     "main",
		Tip:        "tip-oid",
		Commit:     "commit-oid",
		MergeBase:  "base-oid",
		Messages:   Messages{Tip: "tip msg", Commit: "commit msg"},
		Actor:      "agent-7",
		Files: []File{{
			Path:   "a/b.go",
			Kind:   KindContent,
			Base:   &Side{OID: "b1", Mode: ModeRegular, Content: []byte("base\n")},
			Ours:   &Side{OID: "o1", Mode: ModeRegular, Content: []byte{}},
			Theirs: nil,
		}},
		Limits: Limits{MaxFileBytes: 1024},
	}

	raw, err := json.Marshal(req)
	require.NoError(t, err)

	var generic map[string]any
	require.NoError(t, json.Unmarshal(raw, &generic))
	require.Equal(t, float64(1), generic["version"])
	require.Equal(t, "agent-7", generic["actor"])
	require.Equal(t, map[string]any{"max_file_bytes": float64(1024)}, generic["limits"])

	files := generic["files"].([]any)
	file := files[0].(map[string]any)
	require.Nil(t, file["theirs"], "an absent side is JSON null")

	base := file["base"].(map[string]any)
	require.Equal(t, "YmFzZQo=", base["content"], "content is base64")

	ours := file["ours"].(map[string]any)
	require.Equal(t, "", ours["content"], "an empty present side carries an empty string, not null")

	var decoded Request
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, req, decoded)
}

func TestResponseDecodesBase64Content(t *testing.T) {
	t.Parallel()

	var resp Response
	require.NoError(t, json.Unmarshal([]byte(`{
		"version": 1,
		"resolver": "graft-resolve/trivial",
		"files": [{"path": "a/b.go", "action": "write", "mode": "100644", "content": "cGFja2FnZSBhCg=="},
		          {"path": "c.txt", "action": "delete"}],
		"explanation": "free text"
	}`), &resp))

	require.Equal(t, ProtocolVersion, resp.Version)
	require.Equal(t, "graft-resolve/trivial", resp.Resolver)
	require.Equal(t, []byte("package a\n"), resp.Files[0].Content)
	require.Nil(t, resp.Files[1].Content)
	require.Equal(t, "free text", resp.Explanation)
}

func TestSideRejectsInvalidBase64(t *testing.T) {
	t.Parallel()

	var side Side
	require.Error(t, json.Unmarshal([]byte(`{"oid":"x","mode":"100644","content":"!!!"}`), &side))
}
