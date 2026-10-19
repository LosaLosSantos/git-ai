package git

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateRevision(t *testing.T) {
	for _, tc := range []struct {
		desc        string
		revision    string
		opts        []ValidateRevisionOption
		expectedErr error
	}{
		{
			desc:        "empty revision",
			revision:    "",
			expectedErr: fmt.Errorf("empty revision"),
		},
		{
			desc:     "empty revision with allowed empty revisions",
			revision: "",
			opts: []ValidateRevisionOption{
				AllowEmptyRevision(),
			},
		},
		{
			desc:     "valid revision",
			revision: "foo/bar",
		},
		{
			desc:        "leading dash",
			revision:    "-foo/bar",
			expectedErr: fmt.Errorf("revision can't start with '-'"),
		},
		{
			desc:     "intermediate dash",
			revision: "foo-bar",
		},
		{
			desc:        "space",
			revision:    "foo bar",
			expectedErr: fmt.Errorf("revision can't contain whitespace"),
		},
		{
			desc:        "newline",
			revision:    "foo\nbar",
			expectedErr: fmt.Errorf("revision can't contain whitespace"),
		},
		{
			desc:        "tab",
			revision:    "foo\tbar",
			expectedErr: fmt.Errorf("revision can't contain whitespace"),
		},
		{
			desc:        "carriage-return",
			revision:    "foo\rbar",
			expectedErr: fmt.Errorf("revision can't contain whitespace"),
		},
		{
			desc:        "NUL-byte",
			revision:    "foo\x00bar",
			expectedErr: fmt.Errorf("revision can't contain NUL"),
		},
		{
			desc:        "backslash",
			revision:    "foo\\bar\\baz",
			expectedErr: fmt.Errorf("revision can't contain '\\'"),
		},
		{
			desc:        "path-scoped revisions refused by default",
			revision:    "refs/heads/main:path",
			expectedErr: fmt.Errorf("revision can't contain ':'"),
		},
		{
			desc:     "path-scoped revision allowed with option",
			revision: "refs/heads/main:path",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
			},
		},
		{
			desc:     "path-scoped revision does not allow newline in revision",
			revision: "refs/heads\nmain:path",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
			},
			expectedErr: fmt.Errorf("revision can't contain whitespace"),
		},
		{
			desc:     "path-scoped revision must not be empty",
			revision: ":path",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
			},
			expectedErr: fmt.Errorf("empty revision"),
		},
		{
			desc:     "path-scoped path may contain whitespace and special characters",
			revision: "refs/heads/main:path\t :\\\x01\x7f",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
			},
		},
		{
			// Revisions travel to git one per line on its stdin: a line break in the path would be
			// another revision (a reference the request check never saw).
			desc:     "path-scoped path does not allow a line feed",
			revision: "refs/heads/main:path\nrefs/graft/green/main",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
			},
			expectedErr: fmt.Errorf("path of a revision can't contain a line break"),
		},
		{
			// git-cat-file -Z reads revisions NUL-delimited, so its callers may keep a line break.
			desc:     "path-scoped path allows a line break with the option",
			revision: "refs/heads/main:path\nfoo\r",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
				AllowLineBreaksInPath(),
			},
		},
		{
			desc:     "the option allows no line break before the path",
			revision: "refs/heads\nmain:path",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
				AllowLineBreaksInPath(),
			},
			expectedErr: fmt.Errorf("revision can't contain whitespace"),
		},
		{
			desc:     "the option allows no NUL in the path",
			revision: "refs/heads/main:path\x00",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
				AllowLineBreaksInPath(),
			},
			expectedErr: fmt.Errorf("revision can't contain NUL"),
		},
		{
			desc:     "path-scoped path does not allow a carriage return",
			revision: "refs/heads/main:path\r",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
			},
			expectedErr: fmt.Errorf("path of a revision can't contain a line break"),
		},
		{
			desc:     "path-scoped path does not allow NUL",
			revision: "refs/heads/main:path\x00",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
			},
			expectedErr: fmt.Errorf("revision can't contain NUL"),
		},
		{
			desc:        "control character in a revision",
			revision:    "refs/heads/ma\x01in",
			expectedErr: fmt.Errorf("revision can't contain control characters"),
		},
		{
			desc:        "DEL in a revision",
			revision:    "refs/heads/ma\x7fin",
			expectedErr: fmt.Errorf("revision can't contain control characters"),
		},
		{
			desc:     "control character in a path-scoped revision before the path",
			revision: "refs/heads/ma\x01in:path",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
			},
			expectedErr: fmt.Errorf("revision can't contain control characters"),
		},
		{
			desc:     "pseudo-revision pattern with a line feed",
			revision: "--glob=x\nrefs/graft/green/main",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
			expectedErr: fmt.Errorf("revision can't contain control characters"),
		},
		{
			// Not the pseudo-revision --all: a different word, refused as any option is.
			desc:     "pseudo-revision with a carriage return",
			revision: "--all\r",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
			expectedErr: fmt.Errorf("revision can't start with '-'"),
		},
		{
			// Not the pseudo-revision --all: a different word, refused as any option is.
			desc:     "pseudo-revision with NUL",
			revision: "--all\x00",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
			expectedErr: fmt.Errorf("revision can't start with '-'"),
		},
		{
			desc:     "--branches= pattern with a line feed",
			revision: "--branches=x\nrefs/graft/proposals/0123456789abcdef/1",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
				AllowPathScopedRevision(),
			},
			expectedErr: fmt.Errorf("revision can't contain control characters"),
		},
		{
			desc:        "disallowed pseudo-revision",
			revision:    "--all",
			expectedErr: fmt.Errorf("revision can't start with '-'"),
		},
		{
			desc:     "--all",
			revision: "--all",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
		},
		{
			desc:     "--not",
			revision: "--not",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
		},
		{
			desc:     "--branches",
			revision: "--branches",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
		},
		{
			desc:     "--tags",
			revision: "--tags",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
		},
		{
			desc:     "--branches=master",
			revision: "--branches=master",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
		},
		{
			desc:     "--tags=v*",
			revision: "--tags=v*",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
		},
		{
			desc:     "--glob without pattern is refused",
			revision: "--glob",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
			expectedErr: fmt.Errorf("revision can't start with '-'"),
		},
		{
			desc:     "--glob=refs/heads/*",
			revision: "--glob=refs/heads/*",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
		},
		{
			desc:     "--alternate-refs",
			revision: "--alternate-refs",
			opts: []ValidateRevisionOption{
				AllowPseudoRevision(),
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.expectedErr, ValidateRevision([]byte(tc.revision), tc.opts...))
		})
	}
}
