package diff

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestGetPatchID(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupDiffService(t)

	type setupData struct {
		request          *gitalypb.GetPatchIDRequest
		expectedResponse *gitalypb.GetPatchIDResponse
		expectedErr      error
	}

	testCases := []struct {
		desc  string
		setup func(t *testing.T) setupData
	}{
		{
			desc: "returns patch-id successfully",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "old"},
					),
				)
				gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithBranch("main"),
					gittest.WithParents(oldCommit),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "new"},
					),
				)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte("main~"),
						NewRevision: []byte("main"),
					},
					expectedResponse: &gitalypb.GetPatchIDResponse{
						PatchId: "1e1b601449ca8244bcf20f1d8bb3b12b6b899445",
					},
				}
			},
		},
		{
			desc: "returns a different patch-id if the content has additional spaces",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "old"},
					),
				)
				gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithBranch("main"),
					gittest.WithParents(oldCommit),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "  new"},
					),
				)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte("main~"),
						NewRevision: []byte("main"),
					},
					expectedResponse: &gitalypb.GetPatchIDResponse{
						PatchId: "f7ca149454782d7113b74d5b032dd72abd5223fd",
					},
				}
			},
		},
		{
			desc: "returns patch-id successfully with commit ids",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "old"},
					),
				)
				newCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "new"},
					),
				)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte(oldCommit),
						NewRevision: []byte(newCommit),
					},
					expectedResponse: &gitalypb.GetPatchIDResponse{
						PatchId: "1e1b601449ca8244bcf20f1d8bb3b12b6b899445",
					},
				}
			},
		},
		{
			desc: "returns patch-id successfully for a specific file",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "old"},
					),
				)
				newCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "new"},
					),
				)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte(fmt.Sprintf("%s:file", oldCommit)),
						NewRevision: []byte(fmt.Sprintf("%s:file", newCommit)),
					},
					expectedResponse: &gitalypb.GetPatchIDResponse{
						PatchId: "1e1b601449ca8244bcf20f1d8bb3b12b6b899445",
					},
				}
			},
		},
		{
			desc: "returns patch-id with binary file",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "binary", Mode: "100644", Content: "\000old"},
					),
				)
				newCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "binary", Mode: "100644", Content: "\000new"},
					),
				)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte(oldCommit),
						NewRevision: []byte(newCommit),
					},
					expectedResponse: &gitalypb.GetPatchIDResponse{
						PatchId: func() string {
							// Before Git v2.39.0, git-patch-id(1) would skip over any
							// lines that have an "index " prefix. This causes issues
							// with diffs of binaries though: we don't generate the diff
							// with `--binary`, so the "index" line that contains the
							// pre- and post-image blob IDs of the binary is the only
							// bit of information we have that something changed. But
							// because Git used to skip over it we wouldn't actually
							// take into account the contents of the changed blob at
							// all.
							//
							// This was fixed in Git v2.39.0 so that "index" lines will
							// now be hashed to correctly account for binary changes. As
							// a result, the patch ID has changed.
							switch gittest.DefaultObjectHash.Format {
							case "sha1":
								return "f0cecc652860f8dd490cefad3c6e5ab451192acf"
							case "sha256":
								return "860a2112006def90fbb900226d2424bc6e004a61"
							default:
								require.FailNow(t, "unsupported object hash")
								return ""
							}
						}(),
					},
				}
			},
		},
		{
			// This test is essentially the same test set up as the preceding one, but
			// with different binary contents. This is done to ensure that we indeed
			// generate different patch IDs as expected.
			desc: "different binary diff has different patch ID",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "binary", Mode: "100644", Content: "\000old2"},
					),
				)
				newCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "binary", Mode: "100644", Content: "\000new2"},
					),
				)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte(oldCommit),
						NewRevision: []byte(newCommit),
					},
					expectedResponse: &gitalypb.GetPatchIDResponse{
						PatchId: func() string {
							switch gittest.DefaultObjectHash.Format {
							case "sha1":
								return "77171d407829b8297287a7252916269e19573bbb"
							case "sha256":
								return "722dd7c167e4e96c3ff109f7bb52f4436acc544a"
							default:
								require.FailNow(t, "unsupported object hash")
								return ""
							}
						}(),
					},
				}
			},
		},
		{
			desc: "file didn't exist in the old revision",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath)
				newCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "file", Mode: "100644", Content: "new"},
				))

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte(fmt.Sprintf("%s:file", oldCommit)),
						NewRevision: []byte(fmt.Sprintf("%s:file", newCommit)),
					},
					//nolint:gitaly-linters
					expectedErr: testhelper.WithInterceptedMetadata(
						structerr.NewInternal("waiting for git-diff: exit status 128"),
						"stderr", fmt.Sprintf("fatal: path 'file' does not exist in '%s'\n", oldCommit)),
				}
			},
		},
		{
			desc: "unknown revisions",
			setup: func(t *testing.T) setupData {
				repoProto, _ := gittest.CreateRepository(t, ctx, cfg)

				newRevision := strings.Replace(string(gittest.DefaultObjectHash.ZeroOID), "0", "1", -1)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte(gittest.DefaultObjectHash.ZeroOID),
						NewRevision: []byte(newRevision),
					},
					expectedErr: testhelper.WithInterceptedMetadata(
						structerr.NewInternal("waiting for git-diff: exit status 128"),
						"stderr", fmt.Sprintf("fatal: bad object %s\n", gittest.DefaultObjectHash.ZeroOID)),
				}
			},
		},
		{
			desc: "no diff from the given revisions",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				commit := gittest.WriteCommit(t, cfg, repoPath)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte(commit),
						NewRevision: []byte(commit),
					},
					expectedErr: structerr.NewFailedPrecondition("no difference between old and new revision"),
				}
			},
		},
		{
			desc: "empty repository",
			setup: func(t *testing.T) setupData {
				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  nil,
						OldRevision: []byte("HEAD~1"),
						NewRevision: []byte("HEAD"),
					},
					expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
				}
			},
		},
		{
			desc: "empty old revision",
			setup: func(t *testing.T) setupData {
				repoProto, _ := gittest.CreateRepository(t, ctx, cfg)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						NewRevision: []byte("HEAD"),
					},
					expectedErr: structerr.NewInvalidArgument("empty OldRevision"),
				}
			},
		},
		{
			desc: "empty new revision",
			setup: func(t *testing.T) setupData {
				repoProto, _ := gittest.CreateRepository(t, ctx, cfg)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte("HEAD~1"),
					},
					expectedErr: structerr.NewInvalidArgument("empty NewRevision"),
				}
			},
		},
		{
			desc: "context_free produces stable patch-id across different bases",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				// Two bases with the same logical change (TARGET_LINE -> MODIFIED)
				// but different surrounding lines. With default 3-line context,
				// git patch-id hashes context content, so different context =
				// different patch-id. With -U0 only changed lines matter.
				baseContent1 := `ctx1_a
ctx1_b
ctx1_c
TARGET_LINE
ctx1_d
ctx1_e
ctx1_f
`
				changedContent1 := `ctx1_a
ctx1_b
ctx1_c
MODIFIED
ctx1_d
ctx1_e
ctx1_f
`

				base1 := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: baseContent1},
					),
				)
				head1 := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithParents(base1),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: changedContent1},
					),
				)

				baseContent2 := `ctx2_a
ctx2_b
ctx2_c
TARGET_LINE
ctx2_d
ctx2_e
ctx2_f
`
				changedContent2 := `ctx2_a
ctx2_b
ctx2_c
MODIFIED
ctx2_d
ctx2_e
ctx2_f
`

				base2 := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: baseContent2},
					),
				)
				head2 := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithParents(base2),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: changedContent2},
					),
				)

				resp1, err := client.GetPatchID(ctx, &gitalypb.GetPatchIDRequest{
					Repository:  repoProto,
					OldRevision: []byte(base1),
					NewRevision: []byte(head1),
					ContextFree: true,
				})
				require.NoError(t, err)

				resp2, err := client.GetPatchID(ctx, &gitalypb.GetPatchIDRequest{
					Repository:  repoProto,
					OldRevision: []byte(base2),
					NewRevision: []byte(head2),
					ContextFree: true,
				})
				require.NoError(t, err)

				// With context_free, both should produce the same patch-id
				// because the actual change (TARGET_LINE -> MODIFIED) is the same
				require.Equal(t, resp1.GetPatchId(), resp2.GetPatchId(),
					"context_free patch-ids should match for equivalent changes on different bases")

				// Verify that WITHOUT context_free, they differ (different context content is hashed)
				resp3, err := client.GetPatchID(ctx, &gitalypb.GetPatchIDRequest{
					Repository:  repoProto,
					OldRevision: []byte(base1),
					NewRevision: []byte(head1),
					ContextFree: false,
				})
				require.NoError(t, err)

				resp4, err := client.GetPatchID(ctx, &gitalypb.GetPatchIDRequest{
					Repository:  repoProto,
					OldRevision: []byte(base2),
					NewRevision: []byte(head2),
					ContextFree: false,
				})
				require.NoError(t, err)

				require.NotEqual(t, resp3.GetPatchId(), resp4.GetPatchId(),
					"default patch-ids should differ for same changes on different bases due to context shift")

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte(base1),
						NewRevision: []byte(head1),
						ContextFree: true,
					},
					expectedResponse: resp1,
				}
			},
		},
		{
			desc: "context_free still detects actual content changes",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				base := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "original"},
					),
				)
				changeA := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithParents(base),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "change_a"},
					),
				)
				changeB := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithParents(base),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "change_b"},
					),
				)

				respA, err := client.GetPatchID(ctx, &gitalypb.GetPatchIDRequest{
					Repository:  repoProto,
					OldRevision: []byte(base),
					NewRevision: []byte(changeA),
					ContextFree: true,
				})
				require.NoError(t, err)

				respB, err := client.GetPatchID(ctx, &gitalypb.GetPatchIDRequest{
					Repository:  repoProto,
					OldRevision: []byte(base),
					NewRevision: []byte(changeB),
					ContextFree: true,
				})
				require.NoError(t, err)

				require.NotEqual(t, respA.GetPatchId(), respB.GetPatchId(),
					"context_free patch-ids must differ for different actual changes")

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:  repoProto,
						OldRevision: []byte(base),
						NewRevision: []byte(changeA),
						ContextFree: true,
					},
					expectedResponse: respA,
				}
			},
		},
		{
			desc: "whole_diff_hash hashes the whole diff",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "old"},
					),
				)
				newCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "new"},
					),
				)

				// The RPC has to hash exactly the patch that this invocation produces.
				patch := gittest.Exec(t, cfg, "-C", repoPath, "diff", "--full-index",
					oldCommit.String(), newCommit.String())
				expectedPatchID, hashedBytes, err := hashDiff(bytes.NewReader(patch))
				require.NoError(t, err)
				require.NotZero(t, hashedBytes)
				require.Len(t, expectedPatchID, hex.EncodedLen(sha256.Size))

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:    repoProto,
						OldRevision:   []byte(oldCommit),
						NewRevision:   []byte(newCommit),
						WholeDiffHash: true,
					},
					expectedResponse: &gitalypb.GetPatchIDResponse{
						PatchId: expectedPatchID,
					},
				}
			},
		},
		{
			desc: "whole_diff_hash respects context_free",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "a\nb\nc\nd\ne\nf\ng\n"},
					),
				)
				newCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "file", Mode: "100644", Content: "a\nb\nc\nCHANGED\ne\nf\ng\n"},
					),
				)

				patch := gittest.Exec(t, cfg, "-C", repoPath, "diff", "--full-index", "-U0",
					oldCommit.String(), newCommit.String())
				expectedPatchID, _, err := hashDiff(bytes.NewReader(patch))
				require.NoError(t, err)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:    repoProto,
						OldRevision:   []byte(oldCommit),
						NewRevision:   []byte(newCommit),
						ContextFree:   true,
						WholeDiffHash: true,
					},
					expectedResponse: &gitalypb.GetPatchIDResponse{
						PatchId: expectedPatchID,
					},
				}
			},
		},
		{
			desc: "whole_diff_hash with a binary file",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "binary", Mode: "100644", Content: "\000old"},
					),
				)
				newCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "binary", Mode: "100644", Content: "\000new"},
					),
				)

				patch := gittest.Exec(t, cfg, "-C", repoPath, "diff", "--full-index",
					oldCommit.String(), newCommit.String())
				require.Contains(t, string(patch), "Binary files")

				expectedPatchID, hashedBytes, err := hashDiff(bytes.NewReader(patch))
				require.NoError(t, err)
				require.NotZero(t, hashedBytes)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:    repoProto,
						OldRevision:   []byte(oldCommit),
						NewRevision:   []byte(newCommit),
						WholeDiffHash: true,
					},
					expectedResponse: &gitalypb.GetPatchIDResponse{
						PatchId: expectedPatchID,
					},
				}
			},
		},
		{
			// A binary diff has no content to hash, so its patch ID rests entirely on the blob
			// IDs recovered from the "index" line. Two different binary changes must not collide.
			desc: "whole_diff_hash distinguishes different binary changes",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "binary", Mode: "100644", Content: "\000old"},
					),
				)
				firstChange := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "binary", Mode: "100644", Content: "\000first"},
					),
				)
				secondChange := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "binary", Mode: "100644", Content: "\000second"},
					),
				)

				first, err := client.GetPatchID(ctx, &gitalypb.GetPatchIDRequest{
					Repository:    repoProto,
					OldRevision:   []byte(oldCommit),
					NewRevision:   []byte(firstChange),
					WholeDiffHash: true,
				})
				require.NoError(t, err)

				second, err := client.GetPatchID(ctx, &gitalypb.GetPatchIDRequest{
					Repository:    repoProto,
					OldRevision:   []byte(oldCommit),
					NewRevision:   []byte(secondChange),
					WholeDiffHash: true,
				})
				require.NoError(t, err)

				require.NotEqual(t, first.GetPatchId(), second.GetPatchId())

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:    repoProto,
						OldRevision:   []byte(oldCommit),
						NewRevision:   []byte(firstChange),
						WholeDiffHash: true,
					},
					expectedResponse: first,
				}
			},
		},
		{
			// git-diff(1) orders file diffs by path, so the binary file comes first and the text
			// file's header is the one git-patch-id(1) would drop. This exercises that divergence
			// through the RPC.
			desc: "whole_diff_hash with a binary file followed by a text file",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oldCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "binary", Mode: "100644", Content: "\000old"},
						gittest.TreeEntry{Path: "text", Mode: "100644", Content: "old\n"},
					),
				)
				newCommit := gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTreeEntries(
						gittest.TreeEntry{Path: "binary", Mode: "100644", Content: "\000new"},
						gittest.TreeEntry{Path: "text", Mode: "100644", Content: "new\n"},
					),
				)

				patch := gittest.Exec(t, cfg, "-C", repoPath, "diff", "--full-index",
					oldCommit.String(), newCommit.String())
				require.Contains(t, string(patch), "Binary files")

				expectedPatchID, _, err := hashDiff(bytes.NewReader(patch))
				require.NoError(t, err)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:    repoProto,
						OldRevision:   []byte(oldCommit),
						NewRevision:   []byte(newCommit),
						WholeDiffHash: true,
					},
					expectedResponse: &gitalypb.GetPatchIDResponse{
						PatchId: expectedPatchID,
					},
				}
			},
		},
		{
			desc: "whole_diff_hash without a difference between the revisions",
			setup: func(t *testing.T) setupData {
				repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

				commit := gittest.WriteCommit(t, cfg, repoPath)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:    repoProto,
						OldRevision:   []byte(commit),
						NewRevision:   []byte(commit),
						WholeDiffHash: true,
					},
					expectedErr: structerr.NewFailedPrecondition("no difference between old and new revision"),
				}
			},
		},
		{
			desc: "whole_diff_hash with unknown revisions",
			setup: func(t *testing.T) setupData {
				repoProto, _ := gittest.CreateRepository(t, ctx, cfg)

				newRevision := strings.Replace(string(gittest.DefaultObjectHash.ZeroOID), "0", "1", -1)

				return setupData{
					request: &gitalypb.GetPatchIDRequest{
						Repository:    repoProto,
						OldRevision:   []byte(gittest.DefaultObjectHash.ZeroOID),
						NewRevision:   []byte(newRevision),
						WholeDiffHash: true,
					},
					expectedErr: testhelper.WithInterceptedMetadata(
						structerr.NewInternal("waiting for git-diff: exit status 128"),
						"stderr", fmt.Sprintf("fatal: bad object %s\n", gittest.DefaultObjectHash.ZeroOID)),
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			setupData := tc.setup(t)
			response, err := client.GetPatchID(ctx, setupData.request)

			testhelper.RequireGrpcError(t, setupData.expectedErr, err)
			testhelper.ProtoEqual(t, setupData.expectedResponse, response)
		})
	}
}

// hashOfNormalizedDiff computes the digest that hashDiff is expected to return for a patch whose
// normalized form is `normalized`. Expectations are derived from git-patch-id(1)'s normalization
// rules this way rather than from hashDiff itself, so that a bug in the normalization cannot hide
// behind a golden value that was generated by the very code under test.
func hashOfNormalizedDiff(tb testing.TB, normalized string) string {
	tb.Helper()

	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func TestHashDiff(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc string
		// patch is the git-diff(1) output that gets fed to hashDiff.
		patch string
		// normalized is the exact byte stream that must reach the hash.
		normalized string
	}{
		{
			desc: "index line and hunk header are not hashed",
			patch: "diff --git a/file b/file\n" +
				"index 1234567890..0987654321 100644\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"@@ -1 +1 @@\n" +
				"-old\n" +
				"+new\n",
			normalized: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"-old\n" +
				"+new\n",
		},
		{
			// Same change as above at a different offset against a different blob. Because
			// neither the hunk header nor the index line is hashed, this must normalize to
			// exactly the same stream, which is what makes patch IDs survive a rebase.
			desc: "line offsets and blob IDs do not affect the hash",
			patch: "diff --git a/file b/file\n" +
				"index aaaaaaaaaa..bbbbbbbbbb 100644\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"@@ -412,3 +412,3 @@ func someEnclosingFunction() {\n" +
				"-old\n" +
				"+new\n",
			normalized: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"-old\n" +
				"+new\n",
		},
		{
			// Whitespace is what --verbatim preserves, and whitespace-significant languages
			// depend on it: re-indenting a Python statement must change the patch ID.
			desc: "leading whitespace is hashed verbatim",
			patch: "diff --git a/file b/file\n" +
				"index 1234567890..0987654321 100644\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"@@ -1 +1 @@\n" +
				"-    indented\n" +
				"+\tindented\n",
			normalized: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"-    indented\n" +
				"+\tindented\n",
		},
		{
			desc: "trailing whitespace is hashed verbatim",
			patch: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"@@ -1 +1 @@\n" +
				"-old\n" +
				"+new   \n",
			normalized: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"-old\n" +
				"+new   \n",
		},
		{
			desc: "mode changes are hashed",
			patch: "diff --git a/file b/file\n" +
				"old mode 100644\n" +
				"new mode 100755\n",
			normalized: "diff --git a/file b/file\n" +
				"old mode 100644\n" +
				"new mode 100755\n",
		},
		{
			// An index line for an added file carries no mode suffix, which is the branch
			// where git falls back to trimming the final character of the line.
			desc: "added file with a modeless index line",
			patch: "diff --git a/new b/new\n" +
				"new file mode 100644\n" +
				"index 0000000000..1111111111\n" +
				"--- /dev/null\n" +
				"+++ b/new\n" +
				"@@ -0,0 +1 @@\n" +
				"+content\n",
			normalized: "diff --git a/new b/new\n" +
				"new file mode 100644\n" +
				"--- /dev/null\n" +
				"+++ b/new\n" +
				"+content\n",
		},
		{
			desc: "no-newline marker is hashed",
			patch: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"@@ -1 +1 @@\n" +
				"-old\n" +
				"+new\n" +
				"\\ No newline at end of file\n",
			normalized: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"-old\n" +
				"+new\n" +
				"\\ No newline at end of file\n",
		},
		{
			// git-patch-id(1) cannot hash binary content because git-diff(1) is not asked for
			// `--binary`, so it hashes the pre- and post-image blob IDs recovered from the
			// index line instead. This is why the caller must pass --full-index.
			desc: "binary diff hashes the pre- and post-image blob IDs",
			patch: "diff --git a/bin b/bin\n" +
				"index aaaaaaaaaa..bbbbbbbbbb 100644\n" +
				"Binary files a/bin and b/bin differ\n",
			normalized: "diff --git a/bin b/bin\n" +
				"aaaaaaaaaa" + "bbbbbbbbbb",
		},
		{
			// git consumes the file header that terminates a binary hunk without hashing it. We
			// deliberately diverge and hash it, because a file that follows a binary diff without
			// index/---/+++ lines of its own would otherwise contribute nothing path-specific.
			desc: "file header following a binary diff is hashed",
			patch: "diff --git a/bin b/bin\n" +
				"index aaaaaaaaaa..bbbbbbbbbb 100644\n" +
				"Binary files a/bin and b/bin differ\n" +
				"diff --git a/file b/file\n" +
				"index 1234567890..0987654321 100644\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"@@ -1 +1 @@\n" +
				"-old\n" +
				"+new\n",
			normalized: "diff --git a/bin b/bin\n" +
				"aaaaaaaaaa" + "bbbbbbbbbb" +
				"diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"-old\n" +
				"+new\n",
		},
		{
			// A mode-only change produces no index/---/+++ lines, so its file header is the only
			// line that carries its path. git-patch-id(1) drops that header when it follows a
			// binary diff, making patches that chmod different files collide; hashing it is the
			// reason we diverge from git here.
			desc: "mode-only change following a binary diff keeps its path",
			patch: "diff --git a/bin b/bin\n" +
				"index aaaaaaaaaa..bbbbbbbbbb 100644\n" +
				"Binary files a/bin and b/bin differ\n" +
				"diff --git a/script b/script\n" +
				"old mode 100644\n" +
				"new mode 100755\n",
			normalized: "diff --git a/bin b/bin\n" +
				"aaaaaaaaaa" + "bbbbbbbbbb" +
				"diff --git a/script b/script\n" +
				"old mode 100644\n" +
				"new mode 100755\n",
		},
		{
			// The second binary file must hash its own blob IDs, not remnants of the first
			// file's, and its header must contribute its path.
			desc: "consecutive binary diffs",
			patch: "diff --git a/bin1 b/bin1\n" +
				"index aaaaaaaaaa..bbbbbbbbbb 100644\n" +
				"Binary files a/bin1 and b/bin1 differ\n" +
				"diff --git a/bin2 b/bin2\n" +
				"index cccccccccc..dddddddddd 100644\n" +
				"Binary files a/bin2 and b/bin2 differ\n",
			normalized: "diff --git a/bin1 b/bin1\n" +
				"aaaaaaaaaa" + "bbbbbbbbbb" +
				"diff --git a/bin2 b/bin2\n" +
				"cccccccccc" + "dddddddddd",
		},
		{
			// Hunk body lines are prefixed with '+', '-' or ' ', so a diff of a patch file
			// contains lines that look like headers but must be hashed as content. This is
			// what lets hashDiff drop git's before/after line counters.
			desc: "diff of a patch file hashes header-like body lines",
			patch: "diff --git a/x.patch b/x.patch\n" +
				"index 1234567890..0987654321 100644\n" +
				"--- a/x.patch\n" +
				"+++ b/x.patch\n" +
				"@@ -1,4 +1,4 @@\n" +
				" diff --git a/y b/y\n" +
				"-index 1111111111..2222222222 100644\n" +
				"+index 3333333333..4444444444 100644\n" +
				" @@ -1 +1 @@\n" +
				" Binary files a/y and b/y differ\n",
			normalized: "diff --git a/x.patch b/x.patch\n" +
				"--- a/x.patch\n" +
				"+++ b/x.patch\n" +
				" diff --git a/y b/y\n" +
				"-index 1111111111..2222222222 100644\n" +
				"+index 3333333333..4444444444 100644\n" +
				" @@ -1 +1 @@\n" +
				" Binary files a/y and b/y differ\n",
		},
		{
			desc: "final line without a trailing newline is hashed",
			patch: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"@@ -1 +1 @@\n" +
				"-old\n" +
				"+new",
			normalized: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"-old\n" +
				"+new",
		},
		{
			// Minified sources produce single lines far past bufio.Scanner's default token
			// limit, which must not silently truncate what gets hashed.
			desc: "very long lines are hashed in full",
			patch: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"@@ -1 +1 @@\n" +
				"+" + strings.Repeat("x", 128*1024) + "\n",
			normalized: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"+" + strings.Repeat("x", 128*1024) + "\n",
		},
		{
			// A skipped line can be over-long too: `git diff` appends the enclosing function
			// to the hunk header, which for minified sources exceeds the read buffer. Every
			// fragment of it must be skipped, not only the first one.
			desc: "very long hunk header is skipped in full",
			patch: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"@@ -1 +1 @@ " + strings.Repeat("f", 128*1024) + "\n" +
				"-old\n" +
				"+new\n",
			normalized: "diff --git a/file b/file\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"-old\n" +
				"+new\n",
		},
		{
			desc:       "empty patch hashes nothing",
			patch:      "",
			normalized: "",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			patchID, hashedBytes, err := hashDiff(strings.NewReader(tc.patch))
			require.NoError(t, err)
			require.Equal(t, len(tc.normalized), hashedBytes)

			// An empty patch has no patch ID; every other patch hashes to the digest of its
			// normalized form.
			expectedID := hashOfNormalizedDiff(t, tc.normalized)
			if tc.normalized == "" {
				expectedID = ""
			}
			require.Equal(t, expectedID, patchID)
		})
	}
}

func TestHashDiff_malformedBinaryDiff(t *testing.T) {
	t.Parallel()

	// A binary diff carries no content of its own: the blob IDs recovered from the index line are
	// all that goes into the hash. Losing them must be a hard error rather than a silent collision
	// between different binary changes.
	for _, tc := range []struct {
		desc        string
		patch       string
		expectedErr string
	}{
		{
			desc: "binary marker without an index line",
			patch: "diff --git a/bin b/bin\n" +
				"Binary files a/bin and b/bin differ\n",
			expectedErr: "binary diff without index line",
		},
		{
			// The index line of one file must not leak into the next: the header resets it.
			desc: "binary marker whose index line belongs to a previous file",
			patch: "diff --git a/file b/file\n" +
				"index 1234567890..0987654321 100644\n" +
				"--- a/file\n" +
				"+++ b/file\n" +
				"@@ -1 +1 @@\n" +
				"-old\n" +
				"+new\n" +
				"diff --git a/bin b/bin\n" +
				"Binary files a/bin and b/bin differ\n",
			expectedErr: "binary diff without index line",
		},
		{
			desc: "malformed index line",
			patch: "diff --git a/bin b/bin\n" +
				"index garbage\n",
			expectedErr: "malformed index line",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			_, _, err := hashDiff(strings.NewReader(tc.patch))
			require.ErrorContains(t, err, tc.expectedErr)
		})
	}
}

// gitPatchIDDigest hashes b the way git-patch-id(1) does.
//
// git-patch-id(1) hashes with `the_hash_algo`, which would be the object hash of the repository it
// runs in. Gitaly spawns it without a repository though, so the fallback in
// builtin/patch-id.c:cmd_patch_id() applies and the algorithm is always SHA1, even for repositories
// using the SHA256 object format.
func gitPatchIDDigest(tb testing.TB, b []byte) []byte {
	tb.Helper()

	//nolint:gosec // Not a security primitive here: this has to match what git-patch-id(1) does.
	hasher := sha1.New()
	hasher.Write(b)

	return hasher.Sum(nil)
}

// carrySum reproduces the way git-patch-id(1) combines the per-file digests of a patch: each digest
// is added into the accumulator byte by byte with carry (diff.c:flush_one_hunk()). Being a sum, it is
// insensitive to the order of its arguments, which is exactly the property hashDiff does away with.
func carrySum(digests ...[]byte) []byte {
	sum := make([]byte, len(digests[0]))

	for _, digest := range digests {
		var carry uint16
		for i := range sum {
			carry += uint16(sum[i]) + uint16(digest[i])
			sum[i] = byte(carry)
			carry >>= 8
		}
	}

	return sum
}

func TestNormalizeDiff_gitParity(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, _ := setupDiffService(t)

	// git-patch-id(1) finalizes one digest per file diff and sums those digests, so for a patch
	// that touches a single file the sum is just that one digest. `git patch-id --verbatim` is
	// therefore an exact oracle for our normalization: if the two disagree, we are normalizing
	// differently than git does. This holds for single-file patches even though we deliberately
	// diverge from git for multi-file patches, because that divergence only concerns the file
	// header that *follows* a binary diff. Multi-file patches are covered by
	// TestNormalizeDiff_gitParityMultiFile and TestHashDiff.
	for _, tc := range []struct {
		desc       string
		oldEntries []gittest.TreeEntry
		newEntries []gittest.TreeEntry
	}{
		{
			desc:       "modified file",
			oldEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "old\n"}},
			newEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "new\n"}},
		},
		{
			desc:       "added file",
			oldEntries: []gittest.TreeEntry{},
			newEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "added\n"}},
		},
		{
			desc:       "deleted file",
			oldEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "deleted\n"}},
			newEntries: []gittest.TreeEntry{},
		},
		{
			desc:       "mode change only",
			oldEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "same\n"}},
			newEntries: []gittest.TreeEntry{{Path: "file", Mode: "100755", Content: "same\n"}},
		},
		{
			desc:       "binary file",
			oldEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "\000old"}},
			newEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "\000new"}},
		},
		{
			// Exercises the "\ No newline at end of file" marker, which --verbatim hashes but
			// the whitespace-stripping modes drop.
			desc:       "missing trailing newline",
			oldEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "content\n"}},
			newEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "content"}},
		},
		{
			desc:       "whitespace-only change",
			oldEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "    indented\n"}},
			newEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "\tindented\n"}},
		},
		{
			desc:       "trailing whitespace",
			oldEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "trailing\n"}},
			newEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "trailing   \n"}},
		},
		{
			// git-diff(1) is porcelain and detects renames by default, so "similarity index",
			// "rename from" and "rename to" flow through the extended-header handling in
			// production. The edit keeps a hunk in the patch alongside the rename headers.
			desc: "rename with edit",
			oldEntries: []gittest.TreeEntry{{Path: "old-name", Mode: "100644", Content: "" +
				"line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\n"}},
			newEntries: []gittest.TreeEntry{{Path: "new-name", Mode: "100644", Content: "" +
				"line1\nline2\nline3\nline4\nline5\nCHANGED\nline7\nline8\nline9\nline10\n"}},
		},
		{
			// A file whose contents are themselves a patch, so that hunk body lines look like
			// diff headers.
			desc: "content that looks like a patch",
			oldEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "" +
				"diff --git a/y b/y\n" +
				"index 1111111111..2222222222 100644\n" +
				"@@ -1 +1 @@\n" +
				"Binary files a/y and b/y differ\n"}},
			newEntries: []gittest.TreeEntry{{Path: "file", Mode: "100644", Content: "" +
				"diff --git a/y b/y\n" +
				"index 3333333333..4444444444 100644\n" +
				"@@ -1 +1 @@\n" +
				"Binary files a/y and b/y differ\n"}},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			_, repoPath := gittest.CreateRepository(t, ctx, cfg)
			oldCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(tc.oldEntries...))
			newCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(tc.newEntries...))

			// The same invocation that GetPatchID uses.
			patch := gittest.Exec(t, cfg, "-C", repoPath, "diff", "--full-index",
				oldCommit.String(), newCommit.String())
			require.NotEmpty(t, patch)

			var normalized bytes.Buffer
			written, err := normalizeDiff(&normalized, bytes.NewReader(patch))
			require.NoError(t, err)
			require.Equal(t, normalized.Len(), written)

			digests := [][]byte{gitPatchIDDigest(t, normalized.Bytes())}

			// The binary branch of get_one_patchid() finalizes the digest as soon as it sees
			// the "Binary files" line, and the unconditional flush at the end of the function
			// then adds the digest of a freshly initialized, empty hash context on top. A
			// patch whose last file is binary therefore carries one extra empty digest.
			if bytes.Contains(patch, []byte("\nBinary files ")) ||
				bytes.Contains(patch, []byte("\nGIT binary patch")) {
				digests = append(digests, gitPatchIDDigest(t, nil))
			}

			actual := gittest.ExecOpts(t, cfg, gittest.ExecConfig{Stdin: bytes.NewReader(patch)},
				"patch-id", "--verbatim")
			gitPatchID, _, found := strings.Cut(string(actual), " ")
			require.True(t, found, "unexpected git-patch-id output %q", actual)

			require.Equal(t, gitPatchID, hex.EncodeToString(carrySum(digests...)),
				"normalization diverges from git-patch-id --verbatim")
		})
	}
}

func TestNormalizeDiff_gitParityMultiFile(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, _ := setupDiffService(t)

	// For multi-file patches git's sum of per-file digests is no longer the identity, so git can
	// only be used as an oracle by reproducing the sum: the patch is split at its file headers,
	// every file diff is normalized on its own, and the carry-sum of the per-file digests must
	// equal `git patch-id --verbatim` of the whole patch. Splitting first also sidesteps our
	// deliberate divergence on the file header that follows a binary diff, as each file diff
	// then starts with its own header for both implementations. Binary and mode-only changes
	// still cannot take part: git finalizes a file's digest only after a completed hunk, so it
	// folds a binary or hunk-less file diff into its successor's digest, which the sum
	// reproduction here does not model.
	_, repoPath := gittest.CreateRepository(t, ctx, cfg)
	oldCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "deleted", Mode: "100644", Content: "deleted\n"},
		gittest.TreeEntry{Path: "modified", Mode: "100644", Content: "old\n"},
		gittest.TreeEntry{Path: "whitespace", Mode: "100644", Content: "    indented\n"},
	))
	newCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "added", Mode: "100644", Content: "added"},
		gittest.TreeEntry{Path: "modified", Mode: "100644", Content: "new\n"},
		gittest.TreeEntry{Path: "whitespace", Mode: "100644", Content: "\tindented\n"},
	))

	// The same invocation that GetPatchID uses.
	patch := gittest.Exec(t, cfg, "-C", repoPath, "diff", "--full-index",
		oldCommit.String(), newCommit.String())

	var fileDiffs [][]byte
	for _, line := range bytes.SplitAfter(patch, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("diff --git ")) {
			fileDiffs = append(fileDiffs, nil)
		}
		if len(line) > 0 {
			require.NotEmpty(t, fileDiffs, "patch must start with a file header")
			fileDiffs[len(fileDiffs)-1] = append(fileDiffs[len(fileDiffs)-1], line...)
		}
	}

	var digests [][]byte
	for _, fileDiff := range fileDiffs {
		var normalized bytes.Buffer
		_, err := normalizeDiff(&normalized, bytes.NewReader(fileDiff))
		require.NoError(t, err)

		digests = append(digests, gitPatchIDDigest(t, normalized.Bytes()))
	}
	require.Len(t, digests, 4, "expected one file diff per changed path")

	actual := gittest.ExecOpts(t, cfg, gittest.ExecConfig{Stdin: bytes.NewReader(patch)},
		"patch-id", "--verbatim")
	gitPatchID, _, found := strings.Cut(string(actual), " ")
	require.True(t, found, "unexpected git-patch-id output %q", actual)

	require.Equal(t, gitPatchID, hex.EncodeToString(carrySum(digests...)),
		"normalization diverges from git-patch-id --verbatim")
}

func TestHashDiff_fileOrder(t *testing.T) {
	t.Parallel()

	// git-patch-id(1) combines per-file digests with an order-independent sum, so it returns the
	// same ID for both of these. hashDiff exists precisely so that it does not: reordering the
	// file diffs has to change the result.
	fileDiff := func(path string) string {
		return fmt.Sprintf("diff --git a/%[1]s b/%[1]s\n"+
			"index 1234567890..0987654321 100644\n"+
			"--- a/%[1]s\n"+
			"+++ b/%[1]s\n"+
			"@@ -1 +1 @@\n"+
			"-old\n"+
			"+new\n", path)
	}

	firstOrder, _, err := hashDiff(strings.NewReader(fileDiff("a") + fileDiff("b")))
	require.NoError(t, err)

	secondOrder, _, err := hashDiff(strings.NewReader(fileDiff("b") + fileDiff("a")))
	require.NoError(t, err)

	require.NotEqual(t, firstOrder, secondOrder)
}
