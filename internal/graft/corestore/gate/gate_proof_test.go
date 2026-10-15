package gate_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/service/setup"
	"github.com/feder-cr/open_scion/internal/gitlab"
	"github.com/feder-cr/open_scion/internal/graft/corestore"
	"github.com/feder-cr/open_scion/internal/graft/corestore/coretest"
	"github.com/feder-cr/open_scion/internal/graft/corestore/gate"
	"github.com/feder-cr/open_scion/internal/graft/corestore/gitops"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// node is one Gitaly server of the storage core in authoritative mode.
type node struct {
	ctx  context.Context
	cfg  config.Cfg
	core *corestore.Core
	conn *grpc.ClientConn
}

// startNode runs a standalone Gitaly server in authoritative mode with core.
func startNode(t *testing.T, core *corestore.Core) node {
	t.Helper()

	require.True(t, core.Authoritative())

	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
		testserver.WithGraftStorage(core),
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))),
	)

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	// RPCs that read another repository (a start or remote repository) reach it
	// through the server addresses of the context.
	ctx = testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))

	return node{ctx: ctx, cfg: cfg, core: core, conn: gittest.DialService(t, ctx, cfg)}
}

// dbRefs returns the references the database has for the repository.
func (n node) dbRefs(t *testing.T, repo *gitalypb.Repository) map[string]corestore.Target {
	t.Helper()

	rec, err := n.core.Meta.GetRepository(n.ctx, corestore.KeyOf(repo))
	require.NoError(t, err)
	refs, _, err := n.core.Meta.Refs(n.ctx, rec.ID)
	require.NoError(t, err)
	return refs
}

// requireSameRefs checks that the local repository of the node has exactly the
// references that the database has.
func (n node) requireSameRefs(t *testing.T, repo *gitalypb.Repository) {
	t.Helper()

	local, err := gitops.ReadRefs(n.ctx, localrepo.NewTestRepo(t, n.cfg, repo))
	require.NoError(t, err)
	require.Equal(t, n.dbRefs(t, repo), local, "local references must equal the database references")
}

// seed is a repository created through the node, with a commit on main.
type seed struct {
	repo *gitalypb.Repository
	path string
	base git.ObjectID
}

func (n node) newSeed(t *testing.T) seed {
	t.Helper()

	repo, path := gittest.CreateRepository(t, n.ctx, n.cfg, gittest.CreateRepositoryConfig{ClientConn: n.conn})
	base := gittest.WriteCommit(t, n.cfg, path, gittest.WithMessage("base"), gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "a", Mode: "100644", Content: "base\n"},
	))
	n.createBranch(t, repo, "main", base)
	return seed{repo: repo, path: path, base: base}
}

// commit writes a commit object (no reference) on top of parent.
func (n node) commit(t *testing.T, s seed, parent git.ObjectID, message string, entries ...gittest.TreeEntry) git.ObjectID {
	t.Helper()
	return gittest.WriteCommit(t, n.cfg, s.path, gittest.WithParents(parent), gittest.WithMessage(message), gittest.WithTreeEntries(entries...))
}

func (n node) createBranch(t *testing.T, repo *gitalypb.Repository, name string, oid git.ObjectID) {
	t.Helper()
	_, err := gitalypb.NewOperationServiceClient(n.conn).UserCreateBranch(n.ctx, &gitalypb.UserCreateBranchRequest{
		Repository: repo,
		BranchName: []byte(name),
		StartPoint: []byte(oid.String()),
		User:       gittest.TestUser,
	})
	require.NoError(t, err)
}

func (n node) ffBranch(t *testing.T, repo *gitalypb.Repository, name string, oid git.ObjectID) {
	t.Helper()
	_, err := gitalypb.NewOperationServiceClient(n.conn).UserFFBranch(n.ctx, &gitalypb.UserFFBranchRequest{
		Repository: repo,
		User:       gittest.TestUser,
		CommitId:   oid.String(),
		Branch:     []byte(name),
	})
	require.NoError(t, err)
}

func (n node) createReview(t *testing.T, s seed, head git.ObjectID) *gitalypb.Review {
	t.Helper()
	resp, err := gitalypb.NewReviewServiceClient(n.conn).CreateReview(n.ctx, &gitalypb.CreateReviewRequest{
		Repository:   s.repo,
		TargetBranch: []byte("main"),
		HeadCommitId: head.String(),
	})
	require.NoError(t, err)
	return resp.GetReview()
}

func (n node) readCommit(t *testing.T, s seed, oid git.ObjectID) *gitalypb.GitCommit {
	t.Helper()
	commit, err := localrepo.NewTestRepo(t, n.cfg, s.repo).ReadCommit(n.ctx, oid.Revision())
	require.NoError(t, err)
	return commit
}

// receivePackRequest returns the commands and the pack of a push that creates
// ref at oid, in the format git send-pack writes.
func (n node) receivePackRequest(t *testing.T, s seed, ref string, oid git.ObjectID) []byte {
	t.Helper()

	var request bytes.Buffer
	gittest.WritePktlinef(t, &request, "%s %s %s\000 report-status side-band-64k object-format=%s agent=git/2.12.0",
		gittest.DefaultObjectHash.ZeroOID, oid, ref, gittest.DefaultObjectHash.Format)
	gittest.WritePktlineFlush(t, &request)
	request.Write(gittest.ExecOpts(t, n.cfg, gittest.ExecConfig{Stdin: strings.NewReader(oid.String() + "\n")},
		"-C", s.path, "pack-objects", "--stdout", "--revs", "-q"))
	return request.Bytes()
}

// proofCases has one representative call for every entry of
// gate.AuthoritativeMutators. Each case runs on a fresh seed and checks the
// effect of the call; the caller then checks that the local references equal
// the database references.
func proofCases() map[string]func(t *testing.T, n node, s seed) {
	return map[string]func(t *testing.T, n node, s seed){
		"/gitaly.OperationService/UserApplyPatch": func(t *testing.T, n node, s seed) {
			change := n.commit(t, s, s.base, "patched", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "patched\n"})
			patch := gittest.Exec(t, n.cfg, "-C", s.path, "format-patch", "--stdout", s.base.String()+".."+change.String())

			stream, err := gitalypb.NewOperationServiceClient(n.conn).UserApplyPatch(n.ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
				UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Header_{
					Header: &gitalypb.UserApplyPatchRequest_Header{
						Repository:   s.repo,
						User:         gittest.TestUser,
						TargetBranch: []byte("main"),
						Timestamp:    &timestamppb.Timestamp{Seconds: 1234512345},
					},
				},
			}))
			require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
				UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Patches{Patches: patch},
			}))
			resp, err := stream.CloseAndRecv()
			require.NoError(t, err)
			require.Equal(t, corestore.Target(resp.GetBranchUpdate().GetCommitId()), n.dbRefs(t, s.repo)["refs/heads/main"])
			require.NotEqual(t, s.base.String(), resp.GetBranchUpdate().GetCommitId())
		},
		"/gitaly.OperationService/UserCherryPick": func(t *testing.T, n node, s seed) {
			pick := n.commit(t, s, s.base, "pick",
				gittest.TreeEntry{Path: "a", Mode: "100644", Content: "base\n"},
				gittest.TreeEntry{Path: "b", Mode: "100644", Content: "picked\n"})
			resp, err := gitalypb.NewOperationServiceClient(n.conn).UserCherryPick(n.ctx, &gitalypb.UserCherryPickRequest{
				Repository: s.repo,
				User:       gittest.TestUser,
				Commit:     n.readCommit(t, s, pick),
				BranchName: []byte("main"),
				Message:    []byte("cherry-pick"),
			})
			require.NoError(t, err)
			require.Equal(t, corestore.Target(resp.GetBranchUpdate().GetCommitId()), n.dbRefs(t, s.repo)["refs/heads/main"])
		},
		"/gitaly.OperationService/UserCommitFiles": func(t *testing.T, n node, s seed) {
			stream, err := gitalypb.NewOperationServiceClient(n.conn).UserCommitFiles(n.ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UserCommitFilesRequest{
				UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{
					Header: &gitalypb.UserCommitFilesRequestHeader{
						Repository:    s.repo,
						User:          gittest.TestUser,
						BranchName:    []byte("main"),
						CommitMessage: []byte("add a file"),
					},
				},
			}))
			require.NoError(t, stream.Send(&gitalypb.UserCommitFilesRequest{
				UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
					Action: &gitalypb.UserCommitFilesAction{
						UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Header{
							Header: &gitalypb.UserCommitFilesActionHeader{
								Action:   gitalypb.UserCommitFilesActionHeader_CREATE,
								FilePath: []byte("new.txt"),
							},
						},
					},
				},
			}))
			require.NoError(t, stream.Send(&gitalypb.UserCommitFilesRequest{
				UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
					Action: &gitalypb.UserCommitFilesAction{
						UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Content{Content: []byte("new\n")},
					},
				},
			}))
			resp, err := stream.CloseAndRecv()
			require.NoError(t, err)
			require.Equal(t, corestore.Target(resp.GetBranchUpdate().GetCommitId()), n.dbRefs(t, s.repo)["refs/heads/main"])
		},
		"/gitaly.OperationService/UserCreateBranch": func(t *testing.T, n node, s seed) {
			n.createBranch(t, s.repo, "feature", s.base)
			require.Equal(t, corestore.Target(s.base.String()), n.dbRefs(t, s.repo)["refs/heads/feature"])
		},
		"/gitaly.OperationService/UserCreateTag": func(t *testing.T, n node, s seed) {
			resp, err := gitalypb.NewOperationServiceClient(n.conn).UserCreateTag(n.ctx, &gitalypb.UserCreateTagRequest{
				Repository:     s.repo,
				TagName:        []byte("v1.0"),
				User:           gittest.TestUser,
				TargetRevision: []byte("main"),
				Message:        []byte("annotated"),
			})
			require.NoError(t, err)
			require.Equal(t, corestore.Target(resp.GetTag().GetId()), n.dbRefs(t, s.repo)["refs/tags/v1.0"])
		},
		"/gitaly.OperationService/UserDeleteBranch": func(t *testing.T, n node, s seed) {
			n.createBranch(t, s.repo, "gone", s.base)
			_, err := gitalypb.NewOperationServiceClient(n.conn).UserDeleteBranch(n.ctx, &gitalypb.UserDeleteBranchRequest{
				Repository: s.repo,
				BranchName: []byte("gone"),
				User:       gittest.TestUser,
			})
			require.NoError(t, err)
			require.NotContains(t, n.dbRefs(t, s.repo), "refs/heads/gone")
		},
		"/gitaly.OperationService/UserDeleteTag": func(t *testing.T, n node, s seed) {
			client := gitalypb.NewOperationServiceClient(n.conn)
			_, err := client.UserCreateTag(n.ctx, &gitalypb.UserCreateTagRequest{
				Repository:     s.repo,
				TagName:        []byte("gone"),
				User:           gittest.TestUser,
				TargetRevision: []byte("main"),
			})
			require.NoError(t, err)
			require.Contains(t, n.dbRefs(t, s.repo), "refs/tags/gone")

			_, err = client.UserDeleteTag(n.ctx, &gitalypb.UserDeleteTagRequest{
				Repository: s.repo,
				TagName:    []byte("gone"),
				User:       gittest.TestUser,
			})
			require.NoError(t, err)
			require.NotContains(t, n.dbRefs(t, s.repo), "refs/tags/gone")
		},
		"/gitaly.OperationService/UserFFBranch": func(t *testing.T, n node, s seed) {
			child := n.commit(t, s, s.base, "child", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "child\n"})
			n.ffBranch(t, s.repo, "main", child)
			require.Equal(t, corestore.Target(child.String()), n.dbRefs(t, s.repo)["refs/heads/main"])
		},
		"/gitaly.OperationService/UserLand": func(t *testing.T, n node, s seed) {
			child := n.commit(t, s, s.base, "landed", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "landed\n"})
			resp, err := gitalypb.NewOperationServiceClient(n.conn).UserLand(n.ctx, &gitalypb.UserLandRequest{
				Repository: s.repo,
				User:       gittest.TestUser,
				Branch:     []byte("main"),
				CommitId:   child.String(),
			})
			require.NoError(t, err)
			require.Equal(t, corestore.Target(resp.GetLandedCommitId()), n.dbRefs(t, s.repo)["refs/heads/main"])
		},
		"/gitaly.OperationService/UserMergeBranch": func(t *testing.T, n node, s seed) {
			other := n.commit(t, s, s.base, "other",
				gittest.TreeEntry{Path: "a", Mode: "100644", Content: "base\n"},
				gittest.TreeEntry{Path: "b", Mode: "100644", Content: "other\n"})
			ours := n.commit(t, s, s.base, "ours",
				gittest.TreeEntry{Path: "a", Mode: "100644", Content: "base\n"},
				gittest.TreeEntry{Path: "c", Mode: "100644", Content: "ours\n"})
			n.ffBranch(t, s.repo, "main", ours)

			stream, err := gitalypb.NewOperationServiceClient(n.conn).UserMergeBranch(n.ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{
				Repository: s.repo,
				User:       gittest.TestUser,
				CommitId:   other.String(),
				Branch:     []byte("main"),
				Message:    []byte("merge"),
			}))
			first, err := stream.Recv()
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{Apply: true}))
			second, err := stream.Recv()
			require.NoError(t, err)
			require.NoError(t, stream.CloseSend())
			require.Equal(t, first.GetCommitId(), second.GetBranchUpdate().GetCommitId())
			require.Equal(t, corestore.Target(first.GetCommitId()), n.dbRefs(t, s.repo)["refs/heads/main"])
		},
		"/gitaly.OperationService/UserMergeToRef": func(t *testing.T, n node, s seed) {
			other := n.commit(t, s, s.base, "other",
				gittest.TreeEntry{Path: "a", Mode: "100644", Content: "base\n"},
				gittest.TreeEntry{Path: "b", Mode: "100644", Content: "other\n"})
			resp, err := gitalypb.NewOperationServiceClient(n.conn).UserMergeToRef(n.ctx, &gitalypb.UserMergeToRefRequest{
				Repository:     s.repo,
				User:           gittest.TestUser,
				SourceSha:      other.String(),
				FirstParentRef: []byte("refs/heads/main"),
				TargetRef:      []byte("refs/merge-requests/1/merge"),
				Message:        []byte("merge to ref"),
			})
			require.NoError(t, err)
			require.Equal(t, corestore.Target(resp.GetCommitId()), n.dbRefs(t, s.repo)["refs/merge-requests/1/merge"])
		},
		"/gitaly.OperationService/UserRebaseToRef": func(t *testing.T, n node, s seed) {
			other := n.commit(t, s, s.base, "other",
				gittest.TreeEntry{Path: "a", Mode: "100644", Content: "base\n"},
				gittest.TreeEntry{Path: "b", Mode: "100644", Content: "other\n"})
			ours := n.commit(t, s, s.base, "ours",
				gittest.TreeEntry{Path: "a", Mode: "100644", Content: "base\n"},
				gittest.TreeEntry{Path: "c", Mode: "100644", Content: "ours\n"})
			n.ffBranch(t, s.repo, "main", ours)

			resp, err := gitalypb.NewOperationServiceClient(n.conn).UserRebaseToRef(n.ctx, &gitalypb.UserRebaseToRefRequest{
				Repository:     s.repo,
				User:           gittest.TestUser,
				SourceSha:      other.String(),
				FirstParentRef: []byte("refs/heads/main"),
				TargetRef:      []byte("refs/merge-requests/1/train"),
				Timestamp:      &timestamppb.Timestamp{Seconds: 123456},
			})
			require.NoError(t, err)
			require.Equal(t, corestore.Target(resp.GetCommitId()), n.dbRefs(t, s.repo)["refs/merge-requests/1/train"])
		},
		"/gitaly.OperationService/UserRebaseConfirmable": func(t *testing.T, n node, s seed) {
			theirs := n.commit(t, s, s.base, "theirs",
				gittest.TreeEntry{Path: "a", Mode: "100644", Content: "base\n"},
				gittest.TreeEntry{Path: "b", Mode: "100644", Content: "theirs\n"})
			ours := n.commit(t, s, s.base, "ours",
				gittest.TreeEntry{Path: "a", Mode: "100644", Content: "base\n"},
				gittest.TreeEntry{Path: "c", Mode: "100644", Content: "ours\n"})
			n.createBranch(t, s.repo, "theirs", theirs)
			n.createBranch(t, s.repo, "ours", ours)

			stream, err := gitalypb.NewOperationServiceClient(n.conn).UserRebaseConfirmable(n.ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UserRebaseConfirmableRequest{
				UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Header_{
					Header: &gitalypb.UserRebaseConfirmableRequest_Header{
						Repository:       s.repo,
						User:             gittest.TestUser,
						Branch:           []byte("ours"),
						BranchSha:        ours.String(),
						RemoteRepository: s.repo,
						RemoteBranch:     []byte("theirs"),
						Timestamp:        &timestamppb.Timestamp{Seconds: 123456},
					},
				},
			}))
			first, err := stream.Recv()
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UserRebaseConfirmableRequest{
				UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Apply{Apply: true},
			}))
			second, err := stream.Recv()
			require.NoError(t, err)
			require.True(t, second.GetRebaseApplied())
			_, err = stream.Recv()
			require.Equal(t, io.EOF, err)

			require.Equal(t, corestore.Target(first.GetRebaseSha()), n.dbRefs(t, s.repo)["refs/heads/ours"])
			require.NotEqual(t, ours.String(), first.GetRebaseSha())
		},
		"/gitaly.OperationService/UserRevert": func(t *testing.T, n node, s seed) {
			child := n.commit(t, s, s.base, "to revert", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "child\n"})
			n.ffBranch(t, s.repo, "main", child)

			resp, err := gitalypb.NewOperationServiceClient(n.conn).UserRevert(n.ctx, &gitalypb.UserRevertRequest{
				Repository: s.repo,
				User:       gittest.TestUser,
				Commit:     n.readCommit(t, s, child),
				BranchName: []byte("main"),
				Message:    []byte("revert"),
			})
			require.NoError(t, err)
			require.Equal(t, corestore.Target(resp.GetBranchUpdate().GetCommitId()), n.dbRefs(t, s.repo)["refs/heads/main"])
			require.NotEqual(t, child.String(), resp.GetBranchUpdate().GetCommitId())
		},
		"/gitaly.OperationService/UserSquash": func(t *testing.T, n node, s seed) {
			first := n.commit(t, s, s.base, "first", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "first\n"})
			second := n.commit(t, s, first, "second", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "second\n"})
			before := n.dbRefs(t, s.repo)

			resp, err := gitalypb.NewOperationServiceClient(n.conn).UserSquash(n.ctx, &gitalypb.UserSquashRequest{
				Repository:    s.repo,
				User:          gittest.TestUser,
				StartSha:      s.base.String(),
				EndSha:        second.String(),
				Author:        gittest.TestUser,
				CommitMessage: []byte("squashed"),
			})
			require.NoError(t, err)
			require.NotEmpty(t, resp.GetSquashSha())
			require.Equal(t, before, n.dbRefs(t, s.repo), "squashing writes no reference")
		},
		"/gitaly.OperationService/UserUpdateBranch": func(t *testing.T, n node, s seed) {
			child := n.commit(t, s, s.base, "child", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "child\n"})
			n.createBranch(t, s.repo, "updated", s.base)
			_, err := gitalypb.NewOperationServiceClient(n.conn).UserUpdateBranch(n.ctx, &gitalypb.UserUpdateBranchRequest{
				Repository: s.repo,
				BranchName: []byte("updated"),
				User:       gittest.TestUser,
				Newrev:     []byte(child.String()),
				Oldrev:     []byte(s.base.String()),
			})
			require.NoError(t, err)
			require.Equal(t, corestore.Target(child.String()), n.dbRefs(t, s.repo)["refs/heads/updated"])
		},
		"/gitaly.OperationService/UserUpdateSubmodule": func(t *testing.T, n node, s seed) {
			withSubmodule := n.commit(t, s, s.base, "submodule",
				gittest.TreeEntry{Path: "a", Mode: "100644", Content: "base\n"},
				gittest.TreeEntry{Path: ".gitmodules", Mode: "100644", Content: "[submodule \"sub\"]\n\tpath = sub\n\turl = file:///sub\n"},
				gittest.TreeEntry{Path: "sub", Mode: "160000", OID: s.base})
			n.ffBranch(t, s.repo, "main", withSubmodule)

			resp, err := gitalypb.NewOperationServiceClient(n.conn).UserUpdateSubmodule(n.ctx, &gitalypb.UserUpdateSubmoduleRequest{
				Repository:    s.repo,
				User:          gittest.TestUser,
				CommitSha:     withSubmodule.String(),
				Branch:        []byte("main"),
				Submodule:     []byte("sub"),
				CommitMessage: []byte("Updating Submodule: sub"),
			})
			require.NoError(t, err)
			require.Empty(t, resp.GetCommitError())
			require.Equal(t, corestore.Target(resp.GetBranchUpdate().GetCommitId()), n.dbRefs(t, s.repo)["refs/heads/main"])
		},
		"/gitaly.ConflictsService/ResolveConflicts": func(t *testing.T, n node, s seed) {
			ours := n.commit(t, s, s.base, "ours", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "ours\n"})
			theirs := n.commit(t, s, s.base, "theirs", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "theirs\n"})
			n.createBranch(t, s.repo, "ours", ours)
			n.createBranch(t, s.repo, "theirs", theirs)

			filesJSON, err := json.Marshal([]map[string]any{{
				"old_path": "a",
				"new_path": "a",
				"sections": map[string]string{fmt.Sprintf("%x_%d_%d", sha1.Sum([]byte("a")), 1, 1): "head"},
			}})
			require.NoError(t, err)

			stream, err := gitalypb.NewConflictsServiceClient(n.conn).ResolveConflicts(n.ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.ResolveConflictsRequest{
				ResolveConflictsRequestPayload: &gitalypb.ResolveConflictsRequest_Header{
					Header: &gitalypb.ResolveConflictsRequestHeader{
						Repository:       s.repo,
						TargetRepository: s.repo,
						OurCommitOid:     ours.String(),
						TheirCommitOid:   theirs.String(),
						TargetBranch:     []byte("theirs"),
						SourceBranch:     []byte("ours"),
						CommitMessage:    []byte("Solve conflicts"),
						User:             gittest.TestUser,
						Timestamp:        &timestamppb.Timestamp{Seconds: 1234567890},
					},
				},
			}))
			require.NoError(t, stream.Send(&gitalypb.ResolveConflictsRequest{
				ResolveConflictsRequestPayload: &gitalypb.ResolveConflictsRequest_FilesJson{FilesJson: filesJSON},
			}))
			resp, err := stream.CloseAndRecv()
			require.NoError(t, err)
			require.Empty(t, resp.GetResolutionError())
			require.NotEqual(t, corestore.Target(ours.String()), n.dbRefs(t, s.repo)["refs/heads/ours"], "the resolution is committed to the source branch")
		},
		"/gitaly.RefService/DeleteRefs": func(t *testing.T, n node, s seed) {
			n.createBranch(t, s.repo, "deleted", s.base)
			_, err := gitalypb.NewRefServiceClient(n.conn).DeleteRefs(n.ctx, &gitalypb.DeleteRefsRequest{
				Repository: s.repo,
				Refs:       [][]byte{[]byte("refs/heads/deleted")},
			})
			require.NoError(t, err)
			require.NotContains(t, n.dbRefs(t, s.repo), "refs/heads/deleted")
		},
		"/gitaly.RefService/UpdateReferences": func(t *testing.T, n node, s seed) {
			stream, err := gitalypb.NewRefServiceClient(n.conn).UpdateReferences(n.ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UpdateReferencesRequest{
				Repository: s.repo,
				Updates: []*gitalypb.UpdateReferencesRequest_Update{{
					Reference:   []byte("refs/heads/updated"),
					OldObjectId: []byte(gittest.DefaultObjectHash.ZeroOID.String()),
					NewObjectId: []byte(s.base.String()),
				}},
			}))
			_, err = stream.CloseAndRecv()
			require.NoError(t, err)
			require.Equal(t, corestore.Target(s.base.String()), n.dbRefs(t, s.repo)["refs/heads/updated"])
		},
		"/gitaly.RepositoryService/WriteRef": func(t *testing.T, n node, s seed) {
			client := gitalypb.NewRepositoryServiceClient(n.conn)
			_, err := client.WriteRef(n.ctx, &gitalypb.WriteRefRequest{
				Repository: s.repo,
				Ref:        []byte("refs/heads/written"),
				Revision:   []byte(s.base.String()),
			})
			require.NoError(t, err)
			require.Equal(t, corestore.Target(s.base.String()), n.dbRefs(t, s.repo)["refs/heads/written"])

			// WriteRef of HEAD reaches the database through the hook: no post-handler
			// step is needed in the gate.
			_, err = client.WriteRef(n.ctx, &gitalypb.WriteRefRequest{
				Repository: s.repo,
				Ref:        []byte("HEAD"),
				Revision:   []byte("refs/heads/written"),
			})
			require.NoError(t, err)
			require.Equal(t, corestore.SymbolicTarget("refs/heads/written"), n.dbRefs(t, s.repo)["HEAD"])
		},
		"/gitaly.RepositoryService/CreateRepository": func(t *testing.T, n node, s seed) {
			repo := localrepo.NewTestRepo(t, n.cfg, s.repo)
			backend, err := repo.ReferenceBackend(n.ctx)
			require.NoError(t, err)
			require.Equal(t, git.ReferenceBackendReftables.Name, backend.Name, "the gate creates reftable repositories")

			rec, err := n.core.Meta.GetRepository(n.ctx, corestore.KeyOf(s.repo))
			require.NoError(t, err)
			changesets, err := n.core.Meta.ListChangesets(n.ctx, rec.ID, 0, 1)
			require.NoError(t, err)
			require.Len(t, changesets, 1)
			require.Equal(t, "create_repository", changesets[0].Operation)
			require.Equal(t, []corestore.AppliedUpdate{{Name: "HEAD", New: corestore.SymbolicTarget("refs/heads/main")}}, changesets[0].Updates)

			wm, ok, err := gitops.ReadWatermark(s.path)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, rec.ID, wm.RepoID)

			// An explicit reftable request is accepted too.
			other := &gitalypb.Repository{StorageName: s.repo.GetStorageName(), RelativePath: gittest.NewRepositoryName(t)}
			_, err = gitalypb.NewRepositoryServiceClient(n.conn).CreateRepository(n.ctx, &gitalypb.CreateRepositoryRequest{
				Repository:       other,
				ReferenceBackend: gitalypb.ReferenceBackend_REFERENCE_BACKEND_REFTABLE,
			})
			require.NoError(t, err)
			n.requireSameRefs(t, other)
		},
		"/gitaly.RepositoryService/RemoveRepository": func(t *testing.T, n node, s seed) {
			_, err := gitalypb.NewRepositoryServiceClient(n.conn).RemoveRepository(n.ctx, &gitalypb.RemoveRepositoryRequest{Repository: s.repo})
			require.NoError(t, err)
			_, err = n.core.Meta.GetRepository(n.ctx, corestore.KeyOf(s.repo))
			require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
			require.NoDirExists(t, s.path)
		},
		"/gitaly.SSHService/SSHReceivePack": func(t *testing.T, n node, s seed) {
			child := n.commit(t, s, s.base, "pushed over ssh", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "ssh\n"})
			request := n.receivePackRequest(t, s, "refs/heads/ssh-pushed", child)

			stream, err := gitalypb.NewSSHServiceClient(n.conn).SSHReceivePack(n.ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.SSHReceivePackRequest{Repository: s.repo, GlId: "user-123", GlRepository: "project-1"}))
			require.NoError(t, stream.Send(&gitalypb.SSHReceivePackRequest{Stdin: request}))
			require.NoError(t, stream.CloseSend())

			var output bytes.Buffer
			var exitStatus int32
			for {
				resp, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				output.Write(resp.GetStdout())
				output.Write(resp.GetStderr())
				if resp.GetExitStatus() != nil {
					exitStatus = resp.GetExitStatus().GetValue()
				}
			}
			require.Zero(t, exitStatus, output.String())
			require.Equal(t, corestore.Target(child.String()), n.dbRefs(t, s.repo)["refs/heads/ssh-pushed"], output.String())
		},
		"/gitaly.SmartHTTPService/PostReceivePack": func(t *testing.T, n node, s seed) {
			child := n.commit(t, s, s.base, "pushed over http", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "http\n"})
			request := n.receivePackRequest(t, s, "refs/heads/http-pushed", child)

			stream, err := gitalypb.NewSmartHTTPServiceClient(n.conn).PostReceivePack(n.ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.PostReceivePackRequest{Repository: s.repo, GlId: "user-123", GlRepository: "project-1"}))
			require.NoError(t, stream.Send(&gitalypb.PostReceivePackRequest{Data: request}))
			require.NoError(t, stream.CloseSend())

			var output bytes.Buffer
			for {
				resp, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				output.Write(resp.GetData())
			}
			require.Equal(t, corestore.Target(child.String()), n.dbRefs(t, s.repo)["refs/heads/http-pushed"], output.String())
		},
		"/gitaly.ReviewService/CreateReview": func(t *testing.T, n node, s seed) {
			child := n.commit(t, s, s.base, "change", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "change\n"})
			review := n.createReview(t, s, child)
			stored, _, err := n.core.Meta.GetReview(n.ctx, mustRepoID(t, n, s), review.GetId())
			require.NoError(t, err)
			require.Equal(t, child.String(), stored.GetHeadCommitId())
		},
		"/gitaly.ReviewService/UpdateReview": func(t *testing.T, n node, s seed) {
			child := n.commit(t, s, s.base, "change", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "change\n"})
			next := n.commit(t, s, child, "change 2", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "change 2\n"})
			review := n.createReview(t, s, child)
			_, err := gitalypb.NewReviewServiceClient(n.conn).UpdateReview(n.ctx, &gitalypb.UpdateReviewRequest{
				Repository:   s.repo,
				Id:           review.GetId(),
				HeadCommitId: next.String(),
			})
			require.NoError(t, err)
			stored, _, err := n.core.Meta.GetReview(n.ctx, mustRepoID(t, n, s), review.GetId())
			require.NoError(t, err)
			require.Equal(t, next.String(), stored.GetHeadCommitId())
		},
		"/gitaly.ReviewService/AddReviewComment": func(t *testing.T, n node, s seed) {
			review := n.createReview(t, s, n.commit(t, s, s.base, "change", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "change\n"}))
			_, err := gitalypb.NewReviewServiceClient(n.conn).AddReviewComment(n.ctx, &gitalypb.AddReviewCommentRequest{
				Repository: s.repo,
				Id:         review.GetId(),
				Body:       []byte("looks good"),
			})
			require.NoError(t, err)
			stored, _, err := n.core.Meta.GetReview(n.ctx, mustRepoID(t, n, s), review.GetId())
			require.NoError(t, err)
			require.Len(t, stored.GetComments(), 1)
		},
		"/gitaly.ReviewService/SubmitReviewVerdict": func(t *testing.T, n node, s seed) {
			review := n.createReview(t, s, n.commit(t, s, s.base, "change", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "change\n"}))
			_, err := gitalypb.NewReviewServiceClient(n.conn).SubmitReviewVerdict(n.ctx, &gitalypb.SubmitReviewVerdictRequest{
				Repository:   s.repo,
				Id:           review.GetId(),
				Kind:         gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES,
				HeadCommitId: review.GetHeadCommitId(),
			})
			require.NoError(t, err)
			_, version, err := n.core.Meta.GetReview(n.ctx, mustRepoID(t, n, s), review.GetId())
			require.NoError(t, err)
			require.Equal(t, int64(2), version)
		},
		"/gitaly.ReviewService/CloseReview": func(t *testing.T, n node, s seed) {
			review := n.createReview(t, s, n.commit(t, s, s.base, "change", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "change\n"}))
			_, err := gitalypb.NewReviewServiceClient(n.conn).CloseReview(n.ctx, &gitalypb.CloseReviewRequest{
				Repository: s.repo,
				Id:         review.GetId(),
			})
			require.NoError(t, err)
			stored, _, err := n.core.Meta.GetReview(n.ctx, mustRepoID(t, n, s), review.GetId())
			require.NoError(t, err)
			require.Equal(t, gitalypb.Review_STATE_CLOSED, stored.GetState())
		},
	}
}

func mustRepoID(t *testing.T, n node, s seed) int64 {
	t.Helper()
	rec, err := n.core.Meta.GetRepository(n.ctx, corestore.KeyOf(s.repo))
	require.NoError(t, err)
	return rec.ID
}

// TestGateProofAllowlist proves every entry of gate.AuthoritativeMutators: a
// representative call in authoritative mode, after which the local references
// equal the database references.
func TestGateProofAllowlist(t *testing.T) {
	t.Parallel()

	cases := proofCases()

	var names []string
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)

	var allowlisted []string
	for name := range gate.AuthoritativeMutators {
		allowlisted = append(allowlisted, name)
	}
	require.ElementsMatch(t, allowlisted, names, "every allowlist entry has a proof case")

	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			t.Parallel()

			n := startNode(t, coretest.Open(t, coretest.Options{Dialect: dialect}))
			for _, name := range names {
				t.Run(strings.TrimPrefix(name, "/gitaly."), func(t *testing.T) {
					s := n.newSeed(t)
					cases[name](t, n, s)
					if _, err := os.Stat(s.path); err == nil {
						n.requireSameRefs(t, s.repo)
					}
				})
			}
		})
	}
}

// TestGateProofNodes runs two nodes on one database and bucket: a write on
// one node is seen by an accessor on the other, a repository created on one
// node cannot be created again on the other, and it can be removed from a
// node that never served it.
func TestGateProofNodes(t *testing.T) {
	t.Parallel()

	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			t.Parallel()

			databaseURL := coretest.DatabaseURL(t, dialect)
			bucketURL := coretest.BucketURL(t, false)
			a := startNode(t, coretest.Open(t, coretest.Options{Dialect: dialect, DatabaseURL: databaseURL, BucketURL: bucketURL, NodeID: "node-a"}))
			b := startNode(t, coretest.Open(t, coretest.Options{Dialect: dialect, DatabaseURL: databaseURL, BucketURL: bucketURL, NodeID: "node-b"}))

			s := a.newSeed(t)
			repoB := &gitalypb.Repository{StorageName: b.cfg.Storages[0].Name, RelativePath: s.repo.GetRelativePath()}

			findBranch := func(t *testing.T, n node, repo *gitalypb.Repository, name string) string {
				t.Helper()
				resp, err := gitalypb.NewRefServiceClient(n.conn).FindBranch(n.ctx, &gitalypb.FindBranchRequest{Repository: repo, Name: []byte(name)})
				require.NoError(t, err)
				return resp.GetBranch().GetTargetCommit().GetId()
			}

			// Node B has never seen the repository: the accessor hydrates it.
			require.Equal(t, s.base.String(), findBranch(t, b, repoB, "main"))
			b.requireSameRefs(t, repoB)

			// A write on A is seen by the next accessor on B.
			child := a.commit(t, s, s.base, "child", gittest.TreeEntry{Path: "a", Mode: "100644", Content: "child\n"})
			a.ffBranch(t, s.repo, "main", child)
			require.Equal(t, child.String(), findBranch(t, b, repoB, "main"))
			b.requireSameRefs(t, repoB)

			// And the other way round: B lands, A reads.
			bPath := b.cfg.Storages[0].Path + "/" + repoB.GetRelativePath()
			landed := gittest.WriteCommit(t, b.cfg, bPath, gittest.WithParents(child), gittest.WithMessage("from b"),
				gittest.WithTreeEntries(gittest.TreeEntry{Path: "a", Mode: "100644", Content: "from b\n"}))
			resp, err := gitalypb.NewOperationServiceClient(b.conn).UserLand(b.ctx, &gitalypb.UserLandRequest{
				Repository: repoB,
				User:       gittest.TestUser,
				Branch:     []byte("main"),
				CommitId:   landed.String(),
			})
			require.NoError(t, err)
			require.Equal(t, resp.GetLandedCommitId(), findBranch(t, a, s.repo, "main"))
			a.requireSameRefs(t, s.repo)

			// The repository exists in the database: B cannot create it again, and
			// the local repository B created meanwhile does not stay behind.
			other := &gitalypb.Repository{StorageName: b.cfg.Storages[0].Name, RelativePath: gittest.NewRepositoryName(t)}
			_, err = gitalypb.NewRepositoryServiceClient(a.conn).CreateRepository(a.ctx, &gitalypb.CreateRepositoryRequest{Repository: other})
			require.NoError(t, err)
			_, err = gitalypb.NewRepositoryServiceClient(b.conn).CreateRepository(b.ctx, &gitalypb.CreateRepositoryRequest{Repository: other})
			require.Equal(t, codes.AlreadyExists, status.Code(err), "%v", err)
			require.NoDirExists(t, b.cfg.Storages[0].Path+"/"+other.GetRelativePath())

			// B never served it, but removes it from the database.
			_, err = gitalypb.NewRepositoryServiceClient(b.conn).RemoveRepository(b.ctx, &gitalypb.RemoveRepositoryRequest{Repository: other})
			require.NoError(t, err)
			_, err = b.core.Meta.GetRepository(b.ctx, corestore.KeyOf(other))
			require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
			_, err = gitalypb.NewRefServiceClient(b.conn).FindBranch(b.ctx, &gitalypb.FindBranchRequest{Repository: other, Name: []byte("main")})
			require.Equal(t, codes.NotFound, status.Code(err), "%v", err)
		})
	}
}

// TestGateProofRejections covers the RPCs the gate refuses in authoritative
// mode on a real server: mutators outside the allowlist (unary and
// streaming), non-reftable repositories and untracked repositories.
func TestGateProofRejections(t *testing.T) {
	t.Parallel()

	n := startNode(t, coretest.Open(t, coretest.Options{}))
	s := n.newSeed(t)
	repoClient := gitalypb.NewRepositoryServiceClient(n.conn)

	t.Run("unary mutator outside the allowlist", func(t *testing.T) {
		_, err := repoClient.ReplicateRepository(n.ctx, &gitalypb.ReplicateRepositoryRequest{Repository: s.repo, Source: s.repo})
		testhelper.RequireGrpcError(t, structerr.NewFailedPrecondition("/gitaly.RepositoryService/ReplicateRepository is not supported in authoritative storage mode"), err)
	})

	t.Run("streaming mutator outside the allowlist", func(t *testing.T) {
		stream, err := repoClient.CreateRepositoryFromBundle(n.ctx)
		require.NoError(t, err)
		_, err = stream.CloseAndRecv()
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
	})

	t.Run("non-reftable repository", func(t *testing.T) {
		repo := &gitalypb.Repository{StorageName: s.repo.GetStorageName(), RelativePath: gittest.NewRepositoryName(t)}
		_, err := repoClient.CreateRepository(n.ctx, &gitalypb.CreateRepositoryRequest{
			Repository:       repo,
			ReferenceBackend: gitalypb.ReferenceBackend_REFERENCE_BACKEND_FILES,
		})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
		_, err = n.core.Meta.GetRepository(n.ctx, corestore.KeyOf(repo))
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
	})

	t.Run("untracked repository", func(t *testing.T) {
		repo, path := gittest.CreateRepository(t, n.ctx, n.cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
		gittest.WriteCommit(t, n.cfg, path, gittest.WithBranch("main"))

		_, err := gitalypb.NewRefServiceClient(n.conn).FindBranch(n.ctx, &gitalypb.FindBranchRequest{Repository: repo, Name: []byte("main")})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)

		_, err = gitalypb.NewOperationServiceClient(n.conn).UserCreateBranch(n.ctx, &gitalypb.UserCreateBranchRequest{
			Repository: repo,
			BranchName: []byte("other"),
			StartPoint: []byte("main"),
			User:       gittest.TestUser,
		})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
	})

	t.Run("missing repository", func(t *testing.T) {
		repo := &gitalypb.Repository{StorageName: s.repo.GetStorageName(), RelativePath: gittest.NewRepositoryName(t)}
		resp, err := repoClient.RepositoryExists(n.ctx, &gitalypb.RepositoryExistsRequest{Repository: repo})
		require.NoError(t, err)
		require.False(t, resp.GetExists())

		_, err = gitalypb.NewRefServiceClient(n.conn).FindBranch(n.ctx, &gitalypb.FindBranchRequest{Repository: repo, Name: []byte("main")})
		require.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	})

	t.Run("repository created twice on one node", func(t *testing.T) {
		_, err := repoClient.CreateRepository(n.ctx, &gitalypb.CreateRepositoryRequest{Repository: s.repo})
		require.Equal(t, codes.AlreadyExists, status.Code(err), "%v", err)
		require.DirExists(t, s.path, "the existing repository is kept")
		n.requireSameRefs(t, s.repo)
	})
}
