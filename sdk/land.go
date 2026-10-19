package sdk

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// LandStrategy selects how a change that is not a fast-forward is landed.
type LandStrategy = gitalypb.UserLandRequest_Strategy

// Landing strategies.
const (
	// LandRebase rebases the change onto the branch tip, producing linear
	// history. This is the default.
	LandRebase = gitalypb.UserLandRequest_STRATEGY_REBASE_UNSPECIFIED
	// LandMerge creates a merge commit with the branch tip and the change as
	// parents.
	LandMerge = gitalypb.UserLandRequest_STRATEGY_MERGE
	// LandFastForwardOnly only fast-forwards the branch. Otherwise Land fails
	// with a not-ancestor error (see IsNotAncestor).
	LandFastForwardOnly = gitalypb.UserLandRequest_STRATEGY_FF_ONLY
)

// LandOptions describes a change to land on a branch.
type LandOptions struct {
	// Branch is the branch to land on, without the refs/heads/ prefix.
	Branch string
	// Commit is the full object ID of the commit to land.
	Commit string
	// User is the user the change is landed as. It is the committer of
	// rewritten commits.
	User User
	// Strategy selects rebase (default), merge or fast-forward only.
	Strategy LandStrategy
	// MaxAttempts bounds the compare-and-swap retries when the branch moves
	// concurrently. Zero means the server default of 20.
	MaxAttempts int32
	// CreateBranch allows creating Branch at Commit if it does not exist.
	CreateBranch bool
	// MergeMessage is the message of the merge commit for LandMerge. If
	// empty, a default message is used.
	MergeMessage string
	// Timestamp is the committer date of rewritten commits. If zero, the
	// server's current time is used.
	Timestamp time.Time
	// ResolveConflicts asks the server to resolve merge conflicts with its
	// configured conflict resolver (see doc/graft/RESOLVE.md). A resolved
	// landing is a merge commit of the branch tip and the change. If the
	// resolver is disabled, refuses a file or fails, the landing fails with a
	// merge conflict as without this option, and the error carries the
	// conflict package (see LandConflict).
	ResolveConflicts bool
	// Resolution lands Commit with the caller's resolution of the conflicts of
	// a conflict package (see LandConflict and ResolutionFor), as a merge
	// commit of the branch tip and the change. The server accepts it only when
	// it was made against the current tip and resolves exactly the conflicting
	// paths; otherwise Land fails with a fresh package. The request must fit in
	// MaxLandRequestBytes.
	Resolution *Resolution
}

// LandResult describes the outcome of a successful Land call.
type LandResult struct {
	// LandedCommitID is the commit at the branch tip after landing: Commit
	// itself after a fast-forward, otherwise the rebased or merge commit.
	LandedCommitID string
	// OldTip is the branch tip that was replaced. Empty if the branch was
	// created.
	OldTip string
	// Rewritten is true if the server rebased or merged the change.
	Rewritten bool
	// AlreadyLanded is true if Commit was already reachable from the branch
	// tip. The branch was left unchanged.
	AlreadyLanded bool
	// BranchCreated is true if the branch was created by this call.
	BranchCreated bool
	// Attempts is the number of compare-and-swap attempts the server used.
	Attempts int32
	// ResolvedPaths are the conflicting files the resolver resolved, empty if
	// the landing did not need resolution.
	ResolvedPaths []string
	// Resolver is the name of the resolver that resolved the conflicts, such as
	// graft-resolve/trivial. Empty if no conflict was resolved.
	Resolver string
	// Batch is the land batch the change was part of. A change that landed
	// alone has Size 1 and Position 1, see BatchInfo.
	Batch BatchInfo
}

// oldTip maps the zero object ID that the server reports for a created branch
// to the empty string.
func oldTip(oid string) string {
	if strings.Trim(oid, "0") == "" {
		return ""
	}
	return oid
}

// Land lands a commit on a branch on the server. The server rebases or merges
// the change onto the current tip when the branch has moved, and retries
// concurrent updates, so many writers can land changes on the same branch
// without a merge queue.
//
// Failures carry structured details: use IsMergeConflict, IsNotAncestor,
// IsReferenceUpdateConflict, IsRejectedByHook or LandError, and LandConflict
// for the conflict package of a land that asked for resolution.
func (r *Repo) Land(ctx context.Context, opts LandOptions) (*LandResult, error) {
	switch {
	case trimBranchPrefix(opts.Branch) == "":
		return nil, status.Error(codes.InvalidArgument, "branch is required")
	case opts.Commit == "":
		return nil, status.Error(codes.InvalidArgument, "commit is required")
	case opts.MaxAttempts < 0:
		return nil, status.Error(codes.InvalidArgument, "max attempts must not be negative")
	}

	req := &gitalypb.UserLandRequest{
		Repository:       r.repository(),
		User:             opts.User.toProto(),
		Branch:           []byte(trimBranchPrefix(opts.Branch)),
		CommitId:         opts.Commit,
		Strategy:         opts.Strategy,
		MaxAttempts:      opts.MaxAttempts,
		CreateBranch:     opts.CreateBranch,
		MergeMessage:     []byte(opts.MergeMessage),
		ResolveConflicts: opts.ResolveConflicts,
	}
	if !opts.Timestamp.IsZero() {
		req.Timestamp = timestamppb.New(opts.Timestamp)
	}
	if opts.Resolution != nil {
		req.Resolution = opts.Resolution.toProto()
		if err := checkLandRequestSize(req); err != nil {
			return nil, err
		}
	}

	resp, err := r.client.operations.UserLand(ctx, req)
	if err != nil {
		return nil, err
	}

	var resolvedPaths []string
	for _, p := range resp.GetResolvedPaths() {
		resolvedPaths = append(resolvedPaths, string(p))
	}

	return &LandResult{
		LandedCommitID: resp.GetLandedCommitId(),
		OldTip:         oldTip(resp.GetOldTip()),
		Rewritten:      resp.GetRewritten(),
		AlreadyLanded:  resp.GetAlreadyLanded(),
		BranchCreated:  resp.GetBranchUpdate().GetBranchCreated(),
		Attempts:       resp.GetAttempts(),
		ResolvedPaths:  resolvedPaths,
		Resolver:       resp.GetResolver(),
		Batch:          batchOf(resp),
	}, nil
}
