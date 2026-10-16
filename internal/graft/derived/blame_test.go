package derived

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

const (
	sha1A = "0123456789abcdef0123456789abcdef01234567"
	sha1B = "fedcba9876543210fedcba9876543210fedcba98"
	sha2A = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestParsePorcelain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc     string
		input    string
		expected []BlameLine
		errorMsg string
	}{
		{
			desc:     "empty output",
			input:    "",
			expected: nil,
		},
		{
			desc: "single line with metadata",
			input: sha1A + " 1 1 1\n" +
				"author Jane\n" +
				"author-mail <jane@example.com>\n" +
				"summary first\n" +
				"filename file\n" +
				"\thello\n",
			expected: []BlameLine{
				{LineNumber: 1, CommitID: sha1A, OriginalLineNumber: 1, Content: []byte("hello")},
			},
		},
		{
			desc: "group of lines and repeated headers without metadata",
			input: sha1A + " 1 1 2\n" +
				"author Jane\n" +
				"filename file\n" +
				"\tone\n" +
				sha1A + " 2 2\n" +
				"\ttwo\n" +
				sha1B + " 7 3 1\n" +
				"author John\n" +
				"filename file\n" +
				"\tthree\n",
			expected: []BlameLine{
				{LineNumber: 1, CommitID: sha1A, OriginalLineNumber: 1, Content: []byte("one")},
				{LineNumber: 2, CommitID: sha1A, OriginalLineNumber: 2, Content: []byte("two")},
				{LineNumber: 3, CommitID: sha1B, OriginalLineNumber: 7, Content: []byte("three")},
			},
		},
		{
			desc:  "empty content line and content with tabs and spaces",
			input: sha1A + " 1 1\n\t\n" + sha1A + " 2 2\n\tx\ty  z\n",
			expected: []BlameLine{
				{LineNumber: 1, CommitID: sha1A, OriginalLineNumber: 1, Content: []byte("")},
				{LineNumber: 2, CommitID: sha1A, OriginalLineNumber: 2, Content: []byte("x\ty  z")},
			},
		},
		{
			desc:  "SHA-256 object IDs",
			input: sha2A + " 4 9\n\tsha256 line\n",
			expected: []BlameLine{
				{LineNumber: 9, CommitID: sha2A, OriginalLineNumber: 4, Content: []byte("sha256 line")},
			},
		},
		{
			desc:  "lines are sorted by final line number",
			input: sha1A + " 1 2\n\tsecond\n" + sha1B + " 1 1\n\tfirst\n",
			expected: []BlameLine{
				{LineNumber: 1, CommitID: sha1B, OriginalLineNumber: 1, Content: []byte("first")},
				{LineNumber: 2, CommitID: sha1A, OriginalLineNumber: 1, Content: []byte("second")},
			},
		},
		{
			desc:     "content without header",
			input:    "\torphan\n",
			errorMsg: "blame content line without a header",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			lines, err := ParsePorcelain([]byte(tc.input))
			if tc.errorMsg != "" {
				require.ErrorContains(t, err, tc.errorMsg)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expected, lines)
		})
	}
}

func TestParsePorcelain_MatchesGitBlame(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	var content []string
	for i := 0; i < 30; i++ {
		content = append(content, fmt.Sprintf("line %d", i%4))
	}
	commits := []historyCommit{
		{name: "c0", changes: []fileChange{{path: "f", content: strings.Join(content[:10], "\n") + "\n"}}},
		{name: "c1", parents: []string{"c0"}, changes: []fileChange{{path: "f", content: strings.Join(content[:20], "\n") + "\n"}}},
		{name: "c2", parents: []string{"c1"}, changes: []fileChange{{path: "f", content: strings.Join(content, "\n") + "\n"}}},
	}
	ids := writeHistory(t, cfg, repoPath, commits)

	want := gittest.Exec(t, cfg, "-C", repoPath, "blame", "--porcelain", ids["c2"].String(), "--", "f")
	parsed, err := ParsePorcelain(want)
	require.NoError(t, err)
	require.Len(t, parsed, 30)

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	got, err := deriver.Blame(ctx, ids["c2"], "f")
	require.NoError(t, err)
	require.Equal(t, parsed, got)
	require.Equal(t, int64(1), deriver.BlameRuns())

	// The second call is answered from the store.
	again := newTestDeriver(t, ctx, cfg, repo, true)
	got, err = again.Blame(ctx, ids["c2"], "f")
	require.NoError(t, err)
	require.Equal(t, parsed, got)
	require.Zero(t, again.BlameRuns())
}

func TestBlame_FailsForMissingPathAndCorruptEntryIsRecomputed(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, []historyCommit{
		{name: "c0", changes: []fileChange{{path: "f", content: "one\n"}}},
	})

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	_, err := deriver.Blame(ctx, ids["c0"], "missing")
	require.Error(t, err)

	// An unreadable entry is discarded and recomputed.
	require.NoError(t, NewStore(repoPath).Put(kindBlame, Key(kindBlame, ids["c0"], "f"), []byte("not json")))
	got, err := deriver.Blame(ctx, ids["c0"], "f")
	require.NoError(t, err)
	require.Equal(t, []BlameLine{
		{LineNumber: 1, CommitID: ids["c0"].String(), OriginalLineNumber: 1, Content: []byte("one")},
	}, got)

	// The corrupt entry must have been replaced, so that a later query is answered from
	// the store without running git blame again.
	later := newTestDeriver(t, ctx, cfg, repo, true)
	again, err := later.Blame(ctx, ids["c0"], "f")
	require.NoError(t, err)
	require.Equal(t, got, again)
	require.Zero(t, later.BlameRuns(), "the repaired entry must be used")
}

// typeChangeHistory has paths whose file type changes while the blob stays the same,
// and a permission change, which git blame treats differently: a type change ("T")
// finds no origin in the parent, a permission change ("M") passes the blame on.
func typeChangeHistory() []historyCommit {
	return []historyCommit{
		{name: "c0", changes: []fileChange{
			{path: "link", mode: "120000", content: "target"},
			{path: "exe", content: "x\n"},
		}},
		{name: "c1", parents: []string{"c0"}, changes: []fileChange{
			{path: "link", content: "target"},
			{path: "exe", mode: "100755", content: "x\n"},
		}},
		{name: "c2", parents: []string{"c1"}, changes: []fileChange{{path: "link", mode: "120000", content: "target"}}},
		{name: "base", changes: []fileChange{{path: "other", content: "o\n"}}},
		{name: "reg", parents: []string{"base"}, changes: []fileChange{{path: "g", content: "v1\n"}}},
		{name: "sym", parents: []string{"base"}, changes: []fileChange{{path: "g", mode: "120000", content: "v1\n"}}},
		// The first parent holds the same blob as a symlink, the second as a regular file.
		{name: "merge", parents: []string{"sym", "reg"}, changes: []fileChange{{path: "g", content: "v1\n"}}},
	}
}

func TestBlameOrigin_MatchesGitBlameAcrossTypeChanges(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, typeChangeHistory())

	for _, tc := range []struct {
		revision, path, origin string
	}{
		{revision: "c0", path: "link", origin: "c0"},
		{revision: "c1", path: "link", origin: "c1"},
		{revision: "c2", path: "link", origin: "c2"},
		{revision: "c1", path: "exe", origin: "c0"},
		{revision: "c2", path: "exe", origin: "c0"},
		{revision: "sym", path: "g", origin: "sym"},
		{revision: "merge", path: "g", origin: "reg"},
	} {
		want, err := ParsePorcelain(gittest.Exec(t, cfg, "-C", repoPath, "blame", "--porcelain", ids[tc.revision].String(), "--", tc.path))
		require.NoError(t, err)

		for _, cache := range []bool{false, true} {
			deriver := newTestDeriver(t, ctx, cfg, repo, cache)

			origin, err := deriver.BlameOrigin(ctx, ids[tc.revision], tc.path)
			require.NoError(t, err)
			require.Equal(t, ids[tc.origin], origin, "blame origin of %q at %s", tc.path, tc.revision)

			got, err := deriver.Blame(ctx, origin, tc.path)
			require.NoError(t, err)
			require.Equal(t, want, got, "blame of %q at %s", tc.path, tc.revision)
		}
	}
}

func TestBlameOrigin_AbsentPathWritesNothing(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, linearHistory(10, func(int) string { return "" }))

	deriver := newTestDeriver(t, ctx, cfg, repo, true)
	origin, err := deriver.BlameOrigin(ctx, ids["c9"], "missing")
	require.NoError(t, err)
	require.Empty(t, origin)
	require.Equal(t, int64(0), deriver.CommitsComputed(), "an absent path must not be walked")
	require.NoDirExists(t, filepath.Join(repoPath, "graft"))
}
