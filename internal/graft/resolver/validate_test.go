package resolver

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// validationRequest is a request with two conflicted paths and a 1 KiB limit.
func validationRequest() *Request {
	return &Request{
		Version: ProtocolVersion,
		Files: []File{
			{Path: "a/b.go", Kind: KindContent},
			{Path: "c.txt", Kind: KindModifyDelete},
		},
		Limits: Limits{MaxFileBytes: 1024},
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	okFiles := func() []ResponseFile {
		return []ResponseFile{
			{Path: "a/b.go", Action: ActionWrite, Mode: ModeRegular, Content: []byte("package a\n")},
			{Path: "c.txt", Action: ActionDelete},
		}
	}

	for _, tc := range []struct {
		name    string
		mutate  func(resp *Response, req *Request)
		wantErr string
	}{
		{
			name: "valid answer",
		},
		{
			name: "version zero",
			mutate: func(resp *Response, _ *Request) {
				resp.Version = 0
			},
			wantErr: "version 0",
		},
		{
			name: "version two",
			mutate: func(resp *Response, _ *Request) {
				resp.Version = 2
			},
			wantErr: "version 2",
		},
		{
			name: "conflicted path not answered",
			mutate: func(resp *Response, _ *Request) {
				resp.Files = resp.Files[:1]
			},
			wantErr: `"c.txt" is not answered`,
		},
		{
			name: "no files answered",
			mutate: func(resp *Response, _ *Request) {
				resp.Files = nil
			},
			wantErr: `"a/b.go" is not answered`,
		},
		{
			name: "path answered twice",
			mutate: func(resp *Response, _ *Request) {
				resp.Files = append(resp.Files, resp.Files[0])
			},
			wantErr: `"a/b.go" is answered more than once`,
		},
		{
			name: "path that was not conflicted",
			mutate: func(resp *Response, _ *Request) {
				resp.Files = append(resp.Files, ResponseFile{Path: "secret.env", Action: ActionDelete})
			},
			wantErr: `"secret.env" is not a conflicted path`,
		},
		{
			name: "traversal path",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Path = "../../etc/passwd"
			},
			wantErr: "is not a conflicted path",
		},
		{
			name: "unknown action",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Action = "rename"
			},
			wantErr: `invalid action "rename"`,
		},
		{
			name: "empty action",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Action = ""
			},
			wantErr: "invalid action",
		},
		{
			name: "write with directory mode",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Mode = "040000"
			},
			wantErr: `invalid mode "040000" for write`,
		},
		{
			name: "write with empty mode",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Mode = ""
			},
			wantErr: "invalid mode",
		},
		{
			name: "write with world-writable mode",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Mode = "100666"
			},
			wantErr: "invalid mode",
		},
		{
			name: "delete with invalid mode",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[1].Mode = "777"
			},
			wantErr: `invalid mode "777" for delete`,
		},
		{
			name: "delete carrying content",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[1].Content = []byte("x")
			},
			wantErr: "delete must not carry content",
		},
		{
			name: "content over the limit",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Content = make([]byte, 1025)
				for i := range resp.Files[0].Content {
					resp.Files[0].Content[i] = 'x'
				}
			},
			wantErr: "content is 1025 bytes, limit is 1024",
		},
		{
			name: "content exactly at the limit",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Content = make([]byte, 1024)
				for i := range resp.Files[0].Content {
					resp.Files[0].Content[i] = 'x'
				}
			},
		},
		{
			name: "conflict marker with label",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Content = []byte("a\n<<<<<<< HEAD\nb\n=======\nc\n>>>>>>> topic\n")
			},
			wantErr: "conflict marker",
		},
		{
			name: "bare separator marker",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Content = []byte("a\n=======\nb\n")
			},
			wantErr: "conflict marker",
		},
		{
			name: "separator with trailing whitespace",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Content = []byte("a\n======= \nb\n")
			},
			wantErr: "conflict marker",
		},
		{
			name: "marker with CRLF line endings",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Content = []byte("a\r\n>>>>>>> topic\r\nb\r\n")
			},
			wantErr: "conflict marker",
		},
		{
			name: "marker on the last line without newline",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Content = []byte("a\n<<<<<<< ours")
			},
			wantErr: "conflict marker",
		},
		{
			name: "marker text in the middle of a line is allowed",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Content = []byte("x = 1 ======= y\n// <<<<<<< not a marker\nlet s = \">>>>>>>x\"\n========\n<<<<<<<<\n")
			},
		},
		{
			name: "marker inside a binary file is allowed",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Content = []byte("\x00\n<<<<<<< HEAD\n")
			},
		},
		{
			name: "empty written file is allowed",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Content = nil
			},
		},
		{
			name: "symlink mode is allowed",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Mode = ModeSymlink
			},
		},
		{
			name: "executable mode is allowed",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[0].Mode = ModeExecutable
			},
		},
		{
			name: "delete without mode is allowed",
			mutate: func(resp *Response, _ *Request) {
				resp.Files[1].Mode = ""
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := validationRequest()
			resp := &Response{Version: ProtocolVersion, Resolver: "test", Files: okFiles()}
			if tc.mutate != nil {
				tc.mutate(resp, req)
			}

			err := Validate(req, resp)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorIs(t, err, ErrInvalidResponse)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestValidate_NoConflictedPaths(t *testing.T) {
	t.Parallel()

	req := &Request{Version: ProtocolVersion, Limits: Limits{MaxFileBytes: 1}}
	require.NoError(t, Validate(req, &Response{Version: ProtocolVersion}))
	require.ErrorIs(t, Validate(req, &Response{Version: ProtocolVersion, Files: []ResponseFile{{Path: "x", Action: ActionDelete}}}), ErrInvalidResponse)
}

func TestValidate_Errors(t *testing.T) {
	t.Parallel()

	t.Run("nil request", func(t *testing.T) {
		t.Parallel()

		err := Validate(nil, &Response{Version: ProtocolVersion})
		require.ErrorIs(t, err, ErrInvalidResponse)
	})

	t.Run("nil response", func(t *testing.T) {
		t.Parallel()

		err := Validate(validationRequest(), nil)
		require.ErrorIs(t, err, ErrInvalidResponse)
	})

	t.Run("missing limit is a caller error", func(t *testing.T) {
		t.Parallel()

		req := validationRequest()
		req.Limits.MaxFileBytes = 0

		err := Validate(req, &Response{Version: ProtocolVersion})
		require.Error(t, err)
		require.False(t, errors.Is(err, ErrInvalidResponse))
	})
}

func TestValidateFile_specialFileSymlinks(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		".gitmodules", "sub/.gitattributes", ".gitignore", ".mailmap",
		".GITMODULES", ".gitmodules ", ".gitmodules...", ".gitmodules::$DATA",
		"GITMOD~1", "gi7eba~4", "GI7D29~1", "maba30~2", "gitign~1", "gitmod~4 .",
		// The hashed 8.3 forms: fewer letters of the stem, more digits, eight characters in all.
		"gi7eb~12", "gi7e~123", "GI7EBA~9", "mabA3~12.",
		// HFS+ ignores zero-width code points.
		".git\u200cmodules", "\ufeff.gitattributes",
	} {
		err := ValidateFile(ResponseFile{Path: name, Action: ActionWrite, Mode: ModeSymlink, Content: []byte("target")}, 1<<20)
		require.ErrorContains(t, err, "is refused", "%q", name)
		// As a regular file the name is fine.
		require.NoError(t, ValidateFile(ResponseFile{Path: name, Action: ActionWrite, Mode: ModeRegular, Content: []byte("x")}, 1<<20), "%q", name)
	}

	for _, name := range []string{
		".gitmodules-old", "gitmodules", "GITMOD~A", "gitmod~10", "x.gitignore", "link",
		// git's short name takes ~1 to ~4; a hashed form is eight characters with its digits.
		"gitmod~5", "gi7eba~0", "gi7eb~1x", "gi7eba~12x", "gi7ebx~1", "maba3~1.",
	} {
		require.NoError(t, ValidateFile(ResponseFile{Path: name, Action: ActionWrite, Mode: ModeSymlink, Content: []byte("target")}, 1<<20), "%q", name)
	}
}
