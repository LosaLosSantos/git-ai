package operations

import (
	"bytes"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

func TestParseMailInfoHeaders(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc          string
		headers       string
		expectedPatch parsedPatch
		expectedErr   string
	}{
		{
			desc: "all headers",
			headers: `Author: Patch User
Email: patchuser@gitlab.org
Subject: A commit from a patch
Date: Thu, 18 Oct 2018 13:40:35 +0200
`,
			expectedPatch: parsedPatch{
				authorName:  "Patch User",
				authorEmail: "patchuser@gitlab.org",
				authorDate:  time.Date(2018, 10, 18, 13, 40, 35, 0, time.FixedZone("", 2*60*60)),
				subject:     "A commit from a patch",
			},
		},
		{
			desc: "subject with colon",
			headers: `Author: A
Email: a@b.c
Subject: fix: do the thing
Date: Thu, 18 Oct 2018 13:40:35 +0200
`,
			expectedPatch: parsedPatch{
				authorName:  "A",
				authorEmail: "a@b.c",
				authorDate:  time.Date(2018, 10, 18, 13, 40, 35, 0, time.FixedZone("", 2*60*60)),
				subject:     "fix: do the thing",
			},
		},
		{
			desc: "missing author",
			headers: `Email: a@b.c
Subject: subject
Date: Thu, 18 Oct 2018 13:40:35 +0200
`,
			expectedErr: "mail is missing metadata",
		},
		{
			desc: "invalid date",
			headers: `Author: A
Email: a@b.c
Subject: subject
Date: not a date
`,
			expectedErr: "parsing date",
		},
		{
			desc: "trailing empty line terminates parsing",
			headers: `Author: A
Email: a@b.c
Subject: subject
Date: Thu, 18 Oct 2018 13:40:35 +0200

`,
			expectedPatch: parsedPatch{
				authorName:  "A",
				authorEmail: "a@b.c",
				authorDate:  time.Date(2018, 10, 18, 13, 40, 35, 0, time.FixedZone("", 2*60*60)),
				subject:     "subject",
			},
		},
		{
			desc: "metadata after empty line is ignored",
			headers: `Author: A

Email: a@b.c
`,
			expectedErr: "mail is missing metadata",
		},
		{
			desc:        "malformed line",
			headers:     "Author A\n",
			expectedErr: "invalid mail metadata",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			patch, err := parseMailInfoHeaders([]byte(tc.headers))
			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				return
			}

			require.NoError(t, err)
			require.True(t, tc.expectedPatch.authorDate.Equal(patch.authorDate),
				"expected %v, got %v", tc.expectedPatch.authorDate, patch.authorDate)
			patch.authorDate = tc.expectedPatch.authorDate
			require.Equal(t, tc.expectedPatch, patch)
		})
	}
}

func TestSplitAndParseMailbox(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	ctx, cfg, _ := setupOperationsService(t, ctx)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	baseCommit := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Path: "file", Mode: "100644", Content: "base\n"},
		),
	)

	// Build two stacked commits so we can verify that a mailbox with multiple
	// patches is split and parsed in order.
	firstCommit := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithMessage("first subject\n\nfirst body\n"),
		gittest.WithParents(baseCommit),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Path: "file", Mode: "100644", Content: "first\n"},
		),
	)
	secondCommit := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithMessage("second subject\n\nsecond body\n"),
		gittest.WithParents(firstCommit),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Path: "file", Mode: "100644", Content: "second\n"},
		),
	)

	mbox := gittest.Exec(t, cfg, "-C", repoPath, "format-patch", "--stdout",
		fmt.Sprintf("%s..%s", baseCommit.String(), secondCommit.String()))

	workDir := testhelper.TempDir(t)

	patches, err := splitAndParseMailbox(ctx, repo, bytes.NewReader(mbox), workDir)
	require.NoError(t, err)
	require.Len(t, patches, 2)

	require.Equal(t, "first subject", patches[0].subject)
	require.Equal(t, "first body\n", patches[0].body)
	require.Equal(t, "second subject", patches[1].subject)
	require.Equal(t, "second body\n", patches[1].body)

	for _, patch := range patches {
		require.Equal(t, gittest.DefaultCommitterName, patch.authorName)
		require.Equal(t, gittest.DefaultCommitterMail, patch.authorEmail)
		require.False(t, patch.authorDate.IsZero())

		diff, err := os.ReadFile(patch.patchPath)
		require.NoError(t, err)
		require.Contains(t, string(diff), "diff --git a/file b/file")
	}
}
