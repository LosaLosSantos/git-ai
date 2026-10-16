package commit

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestListCommits(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupCommitService(t, ctx)

	repoProto, _ := gittest.CreateRepository(t, ctx, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	committerYear := func(year int) gittest.WriteCommitOption {
		return gittest.WithCommitterDate(time.Date(year, 1, 1, 1, 1, 1, 1, time.UTC))
	}

	// We have a rather simple commit history with a single mainline branch and a feature branch with a single
	// commit that gets merged back into it.
	main0ID, main0 := writeCommit(t, ctx, cfg, repo, gittest.WithMessage("main 0"), committerYear(2000))
	main1ID, main1 := writeCommit(t, ctx, cfg, repo, gittest.WithParents(main0ID), gittest.WithMessage("main 1"), committerYear(2001))
	main2ID, main2 := writeCommit(t, ctx, cfg, repo, gittest.WithParents(main1ID), gittest.WithMessage("main 2"), committerYear(2002))
	main3ID, main3 := writeCommit(t, ctx, cfg, repo, gittest.WithParents(main2ID), gittest.WithMessage("main 3"), committerYear(2003), gittest.WithAuthorName("Different author"))
	main4ID, main4 := writeCommit(t, ctx, cfg, repo, gittest.WithParents(main3ID), gittest.WithMessage("main 4"), committerYear(2004))
	branchingID, branching := writeCommit(t, ctx, cfg, repo, gittest.WithParents(main4ID), gittest.WithMessage("branching"), committerYear(2020))
	featureID, feature := writeCommit(t, ctx, cfg, repo, gittest.WithParents(main4ID), gittest.WithMessage("feature"), committerYear(2005))
	mergeID, merge := writeCommit(t, ctx, cfg, repo, gittest.WithParents(branchingID, featureID), gittest.WithMessage("merge"), committerYear(2006), gittest.WithBranch("branch"))

	// On top of the merge commit we add a file and rename it afterwards so that we can verify
	// that renames are only followed when asked to.
	addFileID, addFile := writeCommit(t, ctx, cfg, repo, gittest.WithParents(mergeID), gittest.WithMessage("add old-name"), committerYear(2007), gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "old-name", Mode: "100644", Content: "content"},
	))
	renameFileID, renameFile := writeCommit(t, ctx, cfg, repo, gittest.WithParents(addFileID), gittest.WithMessage("rename old-name to new-name"), committerYear(2008), gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "new-name", Mode: "100644", Content: "content"},
	))

	for _, tc := range []struct {
		desc            string
		request         *gitalypb.ListCommitsRequest
		expectedCommits []*gitalypb.GitCommit
		expectedErr     error
	}{
		{
			desc: "single revision",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					main3ID.String(),
				},
			},
			expectedCommits: []*gitalypb.GitCommit{
				main3, main2, main1, main0,
			},
		},
		{
			desc: "single revision with limit",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					main3ID.String(),
				},
				PaginationParams: &gitalypb.PaginationParameter{
					Limit: 2,
				},
			},
			expectedCommits: []*gitalypb.GitCommit{
				main3, main2,
			},
		},
		{
			desc: "single revision with page token",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					main3ID.String(),
				},
				PaginationParams: &gitalypb.PaginationParameter{
					PageToken: main1ID.String(),
				},
			},
			expectedCommits: []*gitalypb.GitCommit{
				main0,
			},
		},
		{
			desc: "revision range",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					"^" + main1ID.String(),
					main3ID.String(),
				},
			},
			expectedCommits: []*gitalypb.GitCommit{
				main3, main2,
			},
		},
		{
			desc: "reverse revision range",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					"^" + main1ID.String(),
					main3ID.String(),
				},
				Reverse: true,
			},
			expectedCommits: []*gitalypb.GitCommit{
				main2, main3,
			},
		},
		{
			desc: "revisions with sort topo order",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					mergeID.String(),
				},
				Order: gitalypb.ListCommitsRequest_TOPO,
			},
			expectedCommits: []*gitalypb.GitCommit{
				merge, feature, branching, main4, main3, main2, main1, main0,
			},
		},
		{
			desc: "revisions with sort date order",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					mergeID.String(),
				},
				Order: gitalypb.ListCommitsRequest_DATE,
			},
			expectedCommits: []*gitalypb.GitCommit{
				// Note how branching and feature are swapped compared to the preceding testcase.
				merge, branching, feature, main4, main3, main2, main1, main0,
			},
		},
		{
			desc: "revision with pseudo-revisions",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					mergeID.String(),
					"--not",
					"--all",
				},
			},
			expectedCommits: nil,
		},
		{
			desc: "only non-merge commits",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					mergeID.String(),
				},
				MaxParents: 1,
			},
			expectedCommits: []*gitalypb.GitCommit{
				// The merge commit is missing in this list.
				branching, feature, main4, main3, main2, main1, main0,
			},
		},
		{
			desc: "disabled walk",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					mergeID.String(),
				},
				DisableWalk: true,
			},
			expectedCommits: []*gitalypb.GitCommit{
				merge,
			},
		},
		{
			desc: "first-parent",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					mergeID.String(),
				},
				FirstParent: true,
			},
			expectedCommits: []*gitalypb.GitCommit{
				// We do not see the commit on the feature branch.
				merge, branching, main4, main3, main2, main1, main0,
			},
		},
		{
			desc: "author",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					mergeID.String(),
				},
				Author: []byte("Different author"),
			},
			expectedCommits: []*gitalypb.GitCommit{
				main3,
			},
		},
		{
			desc: "time range",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					mergeID.String(),
				},
				After:  timestamppb.New(time.Date(2002, 1, 1, 1, 1, 1, 1, time.UTC)),
				Before: timestamppb.New(time.Date(2004, 1, 1, 1, 1, 1, 1, time.UTC)),
			},
			expectedCommits: []*gitalypb.GitCommit{
				main4, main3, main2,
			},
		},
		{
			desc: "revisions by multiple message patterns",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					mergeID.String(),
				},
				CommitMessagePatterns: [][]byte{
					[]byte("main 3"),
					[]byte("feature"),
				},
			},
			expectedCommits: []*gitalypb.GitCommit{
				feature, main3,
			},
		},
		{
			desc: "revisions by case insensitive commit message",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					mergeID.String(),
				},
				IgnoreCase: true,
				CommitMessagePatterns: [][]byte{
					[]byte("MAIN 3"),
				},
			},
			expectedCommits: []*gitalypb.GitCommit{
				main3,
			},
		},
		{
			desc: "skip commits",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					main3ID.String(),
				},
				Skip: 2,
			},
			expectedCommits: []*gitalypb.GitCommit{
				// Skip two commits from main3
				main1, main0,
			},
		},
		{
			desc: "empty paths parameter behaves like no filtering",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					main1ID.String(),
				},
				Paths: [][]byte{},
			},
			expectedCommits: []*gitalypb.GitCommit{
				main1, main0,
			},
		},
		{
			desc: "paths parameter with non-existent path returns no commits",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					main1ID.String(),
				},
				Paths: [][]byte{[]byte("non-existent-file.txt")},
			},
			expectedCommits: nil,
		},
		{
			desc: "paths parameter without follow stops at the rename",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					renameFileID.String(),
				},
				Paths:  [][]byte{[]byte("new-name")},
				Follow: false,
			},
			expectedCommits: []*gitalypb.GitCommit{
				renameFile,
			},
		},
		{
			desc: "follow lists the history beyond the rename",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					renameFileID.String(),
				},
				Paths:  [][]byte{[]byte("new-name")},
				Follow: true,
			},
			expectedCommits: []*gitalypb.GitCommit{
				renameFile, addFile,
			},
		},
		{
			desc: "follow with page token continues beyond the rename",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions: []string{
					renameFileID.String(),
				},
				Paths:  [][]byte{[]byte("new-name")},
				Follow: true,
				PaginationParams: &gitalypb.PaginationParameter{
					PageToken: renameFileID.String(),
				},
			},
			expectedCommits: []*gitalypb.GitCommit{
				addFile,
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			stream, err := client.ListCommits(ctx, tc.request)
			require.NoError(t, err)

			var firstResponse *gitalypb.ListCommitsResponse
			commits, err := testhelper.ReceiveAndFold(stream.Recv, func(result []*gitalypb.GitCommit, response *gitalypb.ListCommitsResponse) []*gitalypb.GitCommit {
				if firstResponse == nil {
					firstResponse = response
				}
				return append(result, response.GetCommits()...)
			})
			require.NoError(t, err)
			testhelper.ProtoEqual(t, tc.expectedCommits, commits)
		})
	}

	t.Run("pagination with cursor", func(t *testing.T) {
		t.Parallel()

		// First request with limit
		stream, err := client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{main4ID.String()},
			PaginationParams: &gitalypb.PaginationParameter{
				Limit: 2,
			},
		})
		require.NoError(t, err)

		firstPageCommits, cursor := receiveCommitsWithCursor(t, stream)
		require.Len(t, firstPageCommits, 2)
		testhelper.ProtoEqual(t, []*gitalypb.GitCommit{main4, main3}, firstPageCommits)

		// Verify cursor is returned (in the last response when there are more commits)
		require.NotNil(t, cursor)
		require.Equal(t, main3.GetId(), decodeListCommitsPageToken(cursor.GetNextCursor()))

		// Second request using the cursor
		stream, err = client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{main4ID.String()},
			PaginationParams: &gitalypb.PaginationParameter{
				PageToken: cursor.GetNextCursor(),
				Limit:     2,
			},
		})
		require.NoError(t, err)

		secondPageCommits, err := testhelper.ReceiveAndFold(stream.Recv, func(result []*gitalypb.GitCommit, response *gitalypb.ListCommitsResponse) []*gitalypb.GitCommit {
			return append(result, response.GetCommits()...)
		})
		require.NoError(t, err)
		require.Len(t, secondPageCommits, 2)
		testhelper.ProtoEqual(t, []*gitalypb.GitCommit{main2, main1}, secondPageCommits)
	})

	t.Run("cursor is base64-encoded JSON", func(t *testing.T) {
		t.Parallel()

		stream, err := client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{main4ID.String()},
			PaginationParams: &gitalypb.PaginationParameter{
				Limit: 2,
			},
		})
		require.NoError(t, err)

		_, cursor := receiveCommitsWithCursor(t, stream)
		require.NotNil(t, cursor)

		nextCursor := cursor.GetNextCursor()
		require.NotEqual(t, main3.GetId(), nextCursor, "cursor must not be the raw commit SHA")

		decodedBytes, err := base64.StdEncoding.DecodeString(nextCursor)
		require.NoError(t, err, "cursor should be valid base64")

		var token struct {
			CommitID string `json:"commit_id"`
		}
		err = json.Unmarshal(decodedBytes, &token)
		require.NoError(t, err, "cursor should be valid JSON")
		require.Equal(t, main3.GetId(), token.CommitID)
	})

	t.Run("pagination with legacy raw SHA cursor", func(t *testing.T) {
		t.Parallel()

		// Simulate an old client or rolling upgrade scenario where the
		// page_token is a raw commit SHA rather than an encoded cursor.
		stream, err := client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{main4ID.String()},
			PaginationParams: &gitalypb.PaginationParameter{
				PageToken: main3ID.String(),
				Limit:     2,
			},
		})
		require.NoError(t, err)

		commits, err := testhelper.ReceiveAndFold(stream.Recv, func(result []*gitalypb.GitCommit, response *gitalypb.ListCommitsResponse) []*gitalypb.GitCommit {
			return append(result, response.GetCommits()...)
		})
		require.NoError(t, err)
		require.Len(t, commits, 2)
		testhelper.ProtoEqual(t, []*gitalypb.GitCommit{main2, main1}, commits)
	})

	t.Run("cursor not returned when all commits fit within limit", func(t *testing.T) {
		t.Parallel()

		// main3 has 4 ancestors (main3, main2, main1, main0)
		// Request with limit > 4, should get all commits, no cursor
		stream, err := client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{main3ID.String()},
			PaginationParams: &gitalypb.PaginationParameter{
				Limit: 100,
			},
		})
		require.NoError(t, err)

		commits, cursor := receiveCommitsWithCursor(t, stream)
		require.Len(t, commits, 4)
		require.Nil(t, cursor, "no cursor because all commits were returned")
	})

	t.Run("cursor not returned when exactly limit commits exist", func(t *testing.T) {
		t.Parallel()

		// main3 has exactly 4 commits, request with limit=4
		stream, err := client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{main3ID.String()},
			PaginationParams: &gitalypb.PaginationParameter{
				Limit: 4,
			},
		})
		require.NoError(t, err)

		commits, cursor := receiveCommitsWithCursor(t, stream)
		require.Len(t, commits, 4)
		require.Nil(t, cursor, "no cursor because exactly limit commits exist")
	})

	t.Run("full pagination flow with correct cursor on each page", func(t *testing.T) {
		t.Parallel()

		// main4 has 5 commits (main4, main3, main2, main1, main0)
		// Paginate with limit=2 to test cursor accuracy across pages

		// Page 1: should have cursor (3 more commits exist)
		stream, err := client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{main4ID.String()},
			PaginationParams: &gitalypb.PaginationParameter{
				Limit: 2,
			},
		})
		require.NoError(t, err)

		page1Commits, cursor1 := receiveCommitsWithCursor(t, stream)
		require.Len(t, page1Commits, 2)
		testhelper.ProtoEqual(t, []*gitalypb.GitCommit{main4, main3}, page1Commits)
		require.NotNil(t, cursor1)
		require.Equal(t, main3.GetId(), decodeListCommitsPageToken(cursor1.GetNextCursor()))

		// Page 2: should have cursor (1 more commit exists)
		stream, err = client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{main4ID.String()},
			PaginationParams: &gitalypb.PaginationParameter{
				PageToken: cursor1.GetNextCursor(),
				Limit:     2,
			},
		})
		require.NoError(t, err)

		page2Commits, cursor2 := receiveCommitsWithCursor(t, stream)
		require.Len(t, page2Commits, 2)
		testhelper.ProtoEqual(t, []*gitalypb.GitCommit{main2, main1}, page2Commits)
		require.NotNil(t, cursor2)
		require.Equal(t, main1.GetId(), decodeListCommitsPageToken(cursor2.GetNextCursor()))

		// Page 3 (last): should NOT have cursor
		stream, err = client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{main4ID.String()},
			PaginationParams: &gitalypb.PaginationParameter{
				PageToken: cursor2.GetNextCursor(),
				Limit:     2,
			},
		})
		require.NoError(t, err)

		page3Commits, cursor3 := receiveCommitsWithCursor(t, stream)
		require.Len(t, page3Commits, 1)
		testhelper.ProtoEqual(t, []*gitalypb.GitCommit{main0}, page3Commits)
		require.Nil(t, cursor3, "no cursor on last page")
	})

	t.Run("skip composes with max-count optimization", func(t *testing.T) {
		t.Parallel()

		// git applies --skip before the limit+1 --max-count, so skipping main4
		// of [main4, main3, main2, main1, main0] still leaves main1 and main0
		// beyond the requested page, which must yield a cursor.
		stream, err := client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{main4ID.String()},
			Skip:       1,
			PaginationParams: &gitalypb.PaginationParameter{
				Limit: 2,
			},
		})
		require.NoError(t, err)

		commits, cursor := receiveCommitsWithCursor(t, stream)
		testhelper.ProtoEqual(t, []*gitalypb.GitCommit{main3, main2}, commits)
		require.NotNil(t, cursor)
		require.Equal(t, main2.GetId(), decodeListCommitsPageToken(cursor.GetNextCursor()))
	})

	t.Run("reverse with limit returns oldest commits first", func(t *testing.T) {
		t.Parallel()

		// The limit+1 --max-count optimization must be skipped under reverse:
		// git caps before reversing, so capping would return the newest commits
		// reversed instead of the oldest. Reversed, main4's ancestry is
		// [main0, main1, main2, main3, main4], so the page is [main0, main1]
		// with main2/main3/main4 remaining behind a cursor.
		stream, err := client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{main4ID.String()},
			Reverse:    true,
			PaginationParams: &gitalypb.PaginationParameter{
				Limit: 2,
			},
		})
		require.NoError(t, err)

		commits, cursor := receiveCommitsWithCursor(t, stream)
		testhelper.ProtoEqual(t, []*gitalypb.GitCommit{main0, main1}, commits)
		require.NotNil(t, cursor)
		require.Equal(t, main1.GetId(), decodeListCommitsPageToken(cursor.GetNextCursor()))
	})
}

func TestListCommits_trailers(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupCommitService(t, ctx)

	repoProto, _ := gittest.CreateRepository(t, ctx, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	// A linear history where some commits carry trailers and some do not. This lets us
	// assert that trailers are attached to the right commits and that a commit without
	// trailers has an empty (nil) Trailers field.
	commit0ID, commit0 := writeCommit(t, ctx, cfg, repo, gittest.WithMessage("commit 0\n\nSigned-off-by: Alice <alice@example.com>\n"))
	commit1ID, commit1 := writeCommit(t, ctx, cfg, repo, gittest.WithParents(commit0ID), gittest.WithMessage("commit 1 without trailers\n"))
	commit2ID, commit2 := writeCommit(t, ctx, cfg, repo, gittest.WithParents(commit1ID), gittest.WithMessage("commit 2\n\nSigned-off-by: Bob <bob@example.com>\nCo-authored-by: Carol <carol@example.com>\n"))
	commit3ID, commit3 := writeCommit(t, ctx, cfg, repo, gittest.WithParents(commit2ID), gittest.WithMessage("commit 3 without trailers\n"))

	// Trailers expected on each commit when trailers are requested.
	commit0.Trailers = []*gitalypb.CommitTrailer{
		{Key: []byte("Signed-off-by"), Value: []byte("Alice <alice@example.com>")},
	}
	commit2.Trailers = []*gitalypb.CommitTrailer{
		{Key: []byte("Signed-off-by"), Value: []byte("Bob <bob@example.com>")},
		{Key: []byte("Co-authored-by"), Value: []byte("Carol <carol@example.com>")},
	}

	t.Run("trailers requested returns populated and empty trailers", func(t *testing.T) {
		t.Parallel()

		stream, err := client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{commit3ID.String()},
			Trailers:   true,
		})
		require.NoError(t, err)

		commits, err := testhelper.ReceiveAndFold(stream.Recv, func(result []*gitalypb.GitCommit, response *gitalypb.ListCommitsResponse) []*gitalypb.GitCommit {
			return append(result, response.GetCommits()...)
		})
		require.NoError(t, err)
		testhelper.ProtoEqual(t, []*gitalypb.GitCommit{commit3, commit2, commit1, commit0}, commits)
	})

	t.Run("trailers not requested returns no trailers", func(t *testing.T) {
		t.Parallel()

		stream, err := client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{commit3ID.String()},
		})
		require.NoError(t, err)

		commits, err := testhelper.ReceiveAndFold(stream.Recv, func(result []*gitalypb.GitCommit, response *gitalypb.ListCommitsResponse) []*gitalypb.GitCommit {
			return append(result, response.GetCommits()...)
		})
		require.NoError(t, err)

		for _, commit := range commits {
			require.Nil(t, commit.GetTrailers(), "commit %q should not carry trailers when not requested", commit.GetId())
		}
	})

	t.Run("trailers correct across page boundary", func(t *testing.T) {
		t.Parallel()

		// Page 1 (limit 2): commit3 (no trailers), commit2 (two trailers).
		stream, err := client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{commit3ID.String()},
			Trailers:   true,
			PaginationParams: &gitalypb.PaginationParameter{
				Limit: 2,
			},
		})
		require.NoError(t, err)

		page1, cursor := receiveCommitsWithCursor(t, stream)
		testhelper.ProtoEqual(t, []*gitalypb.GitCommit{commit3, commit2}, page1)
		require.NotNil(t, cursor)

		// Page 2 (limit 2): commit1 (no trailers), commit0 (one trailer).
		stream, err = client.ListCommits(ctx, &gitalypb.ListCommitsRequest{
			Repository: repoProto,
			Revisions:  []string{commit3ID.String()},
			Trailers:   true,
			PaginationParams: &gitalypb.PaginationParameter{
				PageToken: cursor.GetNextCursor(),
				Limit:     2,
			},
		})
		require.NoError(t, err)

		page2, err := testhelper.ReceiveAndFold(stream.Recv, func(result []*gitalypb.GitCommit, response *gitalypb.ListCommitsResponse) []*gitalypb.GitCommit {
			return append(result, response.GetCommits()...)
		})
		require.NoError(t, err)
		testhelper.ProtoEqual(t, []*gitalypb.GitCommit{commit1, commit0}, page2)
	})
}

func TestListCommits_verify(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupCommitService(t, ctx)

	repo, _ := gittest.CreateRepository(t, ctx, cfg)

	for _, tc := range []struct {
		desc        string
		req         *gitalypb.ListCommitsRequest
		expectedErr error
	}{
		{
			desc:        "no repository provided",
			req:         &gitalypb.ListCommitsRequest{Repository: nil},
			expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
		},
		{
			desc:        "no revisions",
			req:         &gitalypb.ListCommitsRequest{Repository: repo},
			expectedErr: structerr.NewInvalidArgument("missing revisions"),
		},
		{
			desc: "invalid revision",
			req:  &gitalypb.ListCommitsRequest{Repository: repo, Revisions: []string{"asdf", "-invalid"}},
			expectedErr: testhelper.WithInterceptedMetadata(
				structerr.NewInvalidArgument("invalid revision: revision can't start with '-'"),
				"revision", "-invalid",
			),
		},
		{
			desc:        "follow without paths",
			req:         &gitalypb.ListCommitsRequest{Repository: repo, Revisions: []string{"HEAD"}, Follow: true},
			expectedErr: structerr.NewInvalidArgument("follow requires exactly one path"),
		},
		{
			desc: "follow with multiple paths",
			req: &gitalypb.ListCommitsRequest{
				Repository: repo,
				Revisions:  []string{"HEAD"},
				Paths:      [][]byte{[]byte("a"), []byte("b")},
				Follow:     true,
			},
			expectedErr: structerr.NewInvalidArgument("follow requires exactly one path"),
		},
		{
			desc: "follow with reverse",
			req: &gitalypb.ListCommitsRequest{
				Repository: repo,
				Revisions:  []string{"HEAD"},
				Paths:      [][]byte{[]byte("a")},
				Follow:     true,
				Reverse:    true,
			},
			expectedErr: structerr.NewInvalidArgument("follow is incompatible with reverse"),
		},
		{
			desc: "follow with skip",
			req: &gitalypb.ListCommitsRequest{
				Repository: repo,
				Revisions:  []string{"HEAD"},
				Paths:      [][]byte{[]byte("a")},
				Follow:     true,
				Skip:       1,
			},
			expectedErr: structerr.NewInvalidArgument("follow is incompatible with skip"),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			stream, err := client.ListCommits(ctx, tc.req)
			require.NoError(t, err)
			_, err = stream.Recv()
			testhelper.RequireGrpcError(t, tc.expectedErr, err)
		})
	}
}

func TestListCommits_globalOptions(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupCommitService(t, ctx)

	repoProto, _ := gittest.CreateRepository(t, ctx, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	commitAID, commitA := writeCommit(t, ctx, cfg, repo, gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "a-foo", Mode: "100644", Content: "a"},
	))
	commitBID, commitB := writeCommit(t, ctx, cfg, repo, gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "a-foo", Mode: "100644", Content: "updated-a"},
		gittest.TreeEntry{Path: "b-foo", Mode: "100644", Content: "b"},
	), gittest.WithParents(commitAID))

	for _, tc := range []struct {
		desc            string
		request         *gitalypb.ListCommitsRequest
		expectedCommits []*gitalypb.GitCommit
	}{
		{
			desc: "glob path without literal pathspecs matches files",
			request: &gitalypb.ListCommitsRequest{
				Repository: repoProto,
				Revisions:  []string{commitBID.String()},
				Paths:      [][]byte{[]byte("a-*")},
			},
			expectedCommits: []*gitalypb.GitCommit{commitB, commitA},
		},
		{
			desc: "glob path with literal pathspecs returns no commits",
			request: &gitalypb.ListCommitsRequest{
				Repository:    repoProto,
				Revisions:     []string{commitBID.String()},
				Paths:         [][]byte{[]byte("a-*")},
				GlobalOptions: &gitalypb.GlobalOptions{LiteralPathspecs: true},
			},
			expectedCommits: nil,
		},
		{
			desc: "exact path with literal pathspecs returns matching commits",
			request: &gitalypb.ListCommitsRequest{
				Repository:    repoProto,
				Revisions:     []string{commitBID.String()},
				Paths:         [][]byte{[]byte("a-foo")},
				GlobalOptions: &gitalypb.GlobalOptions{LiteralPathspecs: true},
			},
			expectedCommits: []*gitalypb.GitCommit{commitB, commitA},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			stream, err := client.ListCommits(ctx, tc.request)
			require.NoError(t, err)

			commits, err := testhelper.ReceiveAndFold(stream.Recv, func(result []*gitalypb.GitCommit, response *gitalypb.ListCommitsResponse) []*gitalypb.GitCommit {
				return append(result, response.GetCommits()...)
			})
			require.NoError(t, err)
			testhelper.ProtoEqual(t, tc.expectedCommits, commits)
		})
	}
}

func TestListCommitsPageToken(t *testing.T) {
	t.Parallel()

	t.Run("round-trip", func(t *testing.T) {
		commitID := "abc123def456789abc123def456789abc123def4"
		encoded, err := encodeListCommitsPageToken(commitID)
		require.NoError(t, err)
		require.NotEqual(t, commitID, encoded, "encoded token should differ from raw ID")
		require.Equal(t, commitID, decodeListCommitsPageToken(encoded))
	})

	t.Run("legacy raw SHA fallback", func(t *testing.T) {
		rawSHA := "abc123def456789abc123def456789abc123def4"
		require.Equal(t, rawSHA, decodeListCommitsPageToken(rawSHA))
	})

	t.Run("non-base64 fallback", func(t *testing.T) {
		token := "not-valid-base64!@#"
		require.Equal(t, token, decodeListCommitsPageToken(token))
	})
}

// receiveCommitsWithCursor collects all commits from the stream and returns
// them along with the pagination cursor. The cursor is sent in the last response
// only when there are more commits beyond the requested limit.
func receiveCommitsWithCursor(t *testing.T, stream gitalypb.CommitService_ListCommitsClient) ([]*gitalypb.GitCommit, *gitalypb.PaginationCursor) {
	t.Helper()
	var cursor *gitalypb.PaginationCursor
	commits, err := testhelper.ReceiveAndFold(stream.Recv, func(result []*gitalypb.GitCommit, response *gitalypb.ListCommitsResponse) []*gitalypb.GitCommit {
		// Cursor is sent in the last response, so we keep updating it
		if response.GetPaginationCursor() != nil {
			cursor = response.GetPaginationCursor()
		}
		return append(result, response.GetCommits()...)
	})
	require.NoError(t, err)
	return commits, cursor
}
