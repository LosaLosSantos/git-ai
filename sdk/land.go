package sdk

import (
	"context"
	"math/rand/v2"
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
	// merge conflict as without this option.
	ResolveConflicts bool
	// RetryUnavailable is how Land retries a call that the server answers
	// with Unavailable (see UnavailableRetry). It is on by default for
	// LandMerge and LandFastForwardOnly, and off for LandRebase.
	RetryUnavailable UnavailableRetry
}

// UnavailableRetry is the client side of the server's contract for
// Unavailable: a node that cannot serve a land right now (its storage core
// sync ran out of sync_timeout, its land lane was closed) answers Unavailable,
// and the client retries later (doc/graft/LAND-BATCHING.md, Limits). Land
// retries only codes.Unavailable, never another code, with an exponential
// backoff with full jitter: before retry n it waits a random time between zero
// and min(Cap, Base x 2^(n-1)). A context that ends stops the waiting.
//
// The response of a call can be lost after the server landed the change. With
// LandMerge and LandFastForwardOnly, Commit is then reachable from the tip, and
// the retry is reported as AlreadyLanded: these lands are retried by default.
// With LandRebase, ResolveConflicts included (a land without conflicts is a
// plain rebase), the branch holds a rebased copy, not Commit itself, and the
// server cannot tell yet that the change landed: the retry replays the change
// onto the tip. Every commit of it comes out empty and is dropped, unless a
// later commit reverted the change in between, for example graft-verify
// reverting the culprit: the retry would then land the change again. Rebase
// lands are therefore not retried unless RebaseLands is set by a caller that
// accepts this. A retried land with ResolveConflicts may run the conflict
// resolver again, a model included, for the same conflict; LandResult reports
// the retries (UnavailableRetries), so that this cost is visible.
type UnavailableRetry struct {
	// Disabled turns the retries off: Land returns Unavailable at once.
	Disabled bool
	// RebaseLands also retries LandRebase lands, which a retry after a lost
	// response may land again when the change was reverted in between.
	RebaseLands bool
	// MaxRetries bounds the retries after the first call. Zero means 5.
	MaxRetries int
	// Base is the backoff of the first retry. Zero means 100ms.
	Base time.Duration
	// Cap bounds the backoff of every retry. Zero means 2s.
	Cap time.Duration
}

// Defaults of UnavailableRetry.
const (
	defaultUnavailableRetries = 5
	defaultUnavailableBase    = 100 * time.Millisecond
	defaultUnavailableCap     = 2 * time.Second
)

// backoff returns the random wait before retry n (n >= 1).
func (r UnavailableRetry) backoff(n int) time.Duration {
	base, maxBackoff := r.Base, r.Cap
	if base <= 0 {
		base = defaultUnavailableBase
	}
	if maxBackoff <= 0 {
		maxBackoff = defaultUnavailableCap
	}
	limit := base
	for i := 1; i < n && limit < maxBackoff; i++ {
		limit *= 2
	}
	limit = min(limit, maxBackoff)
	return time.Duration(rand.Int64N(int64(limit) + 1))
}

// maxRetries returns the retries of a land with the given strategy.
func (r UnavailableRetry) maxRetries(strategy LandStrategy) int {
	switch {
	case r.Disabled, strategy == LandRebase && !r.RebaseLands:
		return 0
	case r.MaxRetries > 0:
		return r.MaxRetries
	default:
		return defaultUnavailableRetries
	}
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
	// UnavailableRetries is the number of calls the server answered with
	// Unavailable before this one succeeded (see UnavailableRetry).
	UnavailableRetries int
	// LastUnavailable is the message of the last of those answers, empty
	// without retries.
	LastUnavailable string
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
// An Unavailable answer is retried as opts.RetryUnavailable says; when the
// retries run out, the last Unavailable error is returned.
//
// Failures carry structured details: use IsMergeConflict, IsNotAncestor,
// IsReferenceUpdateConflict, IsRejectedByHook or LandError.
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

	var (
		resp            *gitalypb.UserLandResponse
		err             error
		retries         int
		lastUnavailable string
	)
	for {
		resp, err = r.client.operations.UserLand(ctx, req)
		if status.Code(err) != codes.Unavailable || retries >= opts.RetryUnavailable.maxRetries(opts.Strategy) {
			break
		}
		retries++
		lastUnavailable = status.Convert(err).Message()

		timer := time.NewTimer(opts.RetryUnavailable.backoff(retries))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-timer.C:
		}
	}
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

		UnavailableRetries: retries,
		LastUnavailable:    lastUnavailable,
	}, nil
}
