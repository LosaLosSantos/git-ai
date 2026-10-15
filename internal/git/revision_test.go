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
			desc:     "path-scoped path may contain arbitrary characters",
			revision: "refs/heads/main:path\n\t\r :\\",
			opts: []ValidateRevisionOption{
				AllowPathScopedRevision(),
			},
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
