package operations

import (
	"context"
	"errors"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/graft/ancestry"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/landlane"
	"github.com/feder-cr/git-ai/internal/graft/rebase"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// landBatchLanes batches the UserLand requests of a branch. Requests of one branch of one repository
// share a lane, and each batch of a lane is executed by Server.executeLandBatch.
type landBatchLanes = landlane.Registry[*landBatchRequest, *gitalypb.UserLandResponse]

// newLandBatchLanes returns the lanes of UserLand. It returns nil, which disables batching, when the
// batching is disabled or its configuration is invalid.
//
// The lanes are those of the process (see service.Dependencies.GraftLandLanes), which all servers of
// the process share, so that the requests for one branch run in one lane whichever listener they
// arrive on. The first server builds them with its executor. The servers are built from the same
// dependencies, so the executor of any of them lands the batches of all of them. Dependencies
// without lanes, which tests build, get lanes of this server only, from [graft.land.batch].
func newLandBatchLanes(s *Server, cfg config.Graft, shared *landlane.Shared, logger log.Logger) *landBatchLanes {
	if cfg.Land.Batch.Disabled {
		return nil
	}

	if shared == nil {
		laneCfg, err := landlane.ConfigFromGraft(cfg.Land.Batch)
		if err != nil {
			logger.WithError(err).Error("graft: land batching is disabled")
			return nil
		}

		shared = landlane.NewShared(laneCfg)
	}

	lanes, err := landlane.SharedRegistry(shared, s.executeLandBatch)
	if err != nil {
		logger.WithError(err).Error("graft: land batching is disabled")
		return nil
	}

	return lanes
}

// landBatchable reports whether the request lands through its lane. Batching applies to standalone
// Graft only, and not to MVCC snapshots, which are pinned to the tip they were taken at. A request
// for a missing branch lands alone, as it creates the branch. A request with its own object
// directories lands alone too: the quarantine of a batch is built from its first request, so the
// objects that only the request's directories hold would not be visible to the batch.
func (s *Server) landBatchable(ctx context.Context, in *gitalypb.UserLandRequest) bool {
	if s.landLanes == nil || !s.graft.Standalone || mvcc.CacheFromContext(ctx) != nil {
		return false
	}

	repo := in.GetRepository()
	if repo.GetGitObjectDirectory() != "" || len(repo.GetGitAlternateObjectDirectories()) > 0 {
		return false
	}

	reference := git.NewReferenceNameFromBranchName(string(in.GetBranch()))
	if _, err := s.localRepoFactory.Build(in.GetRepository()).ResolveRevision(ctx, reference.Revision()); err != nil {
		return false
	}

	return true
}

// userLandBatched lands the request through its lane. The response is built from the outcome of
// the batch that executed the request.
func (s *Server) userLandBatched(ctx context.Context, in *gitalypb.UserLandRequest, committer git.Signature) (*gitalypb.UserLandResponse, error) {
	repo := in.GetRepository()
	key := landlane.Key{
		Storage:      repo.GetStorageName(),
		RelativePath: repo.GetRelativePath(),
		Branch:       string(in.GetBranch()),
	}

	req := &landBatchRequest{
		ctx:       ctx,
		in:        in,
		committer: committer,
		actor:     landBatchActor(ctx, in),
	}

	resp, err := s.landLanes.Submit(ctx, key, req)
	if err != nil {
		return nil, batchSubmitError(err)
	}

	if resp.Value != nil && resp.Value.GetBatchSize() == 0 {
		// The request was delivered before its batch ended (see executeLandBatch), so the batch did not
		// report its size and position in the value. The lane reports them.
		resp.Value.BatchSize = int32(resp.BatchSize)
		resp.Value.BatchPosition = int32(resp.BatchPosition + 1)
	}

	return resp.Value, nil
}

// batchSubmitError returns the gRPC error of a request that did not get the result of its batch.
// A request whose context ended is reported with the code of the reason it ended.
func batchSubmitError(err error) error {
	switch {
	case errors.Is(err, landlane.ErrClosed):
		return structerr.NewUnavailable("land batching is shut down: %w", err)
	case errors.Is(err, context.DeadlineExceeded):
		return structerr.NewDeadlineExceeded("waiting for landing batch: %w", err)
	case errors.Is(err, context.Canceled):
		return structerr.NewCanceled("waiting for landing batch: %w", err)
	}

	return err
}

// executeLandBatch lands the requests of one lane as a batch. It is the executor of the lane registry,
// and the lane runs one batch at a time. The context is the lane context, which is not cancelled when
// a submitter cancels its request.
func (s *Server) executeLandBatch(ctx context.Context, _ landlane.Key, reqs []*landBatchRequest) ([]landlane.Result[*gitalypb.UserLandResponse], error) {
	defer observeLandBatchPhase(landBatchPhaseTotal, time.Now())

	repository := reqs[0].in.GetRepository()

	executor := landBatchExecutor{
		maxRebuilds: landBatchMaxRebuilds,
		newStepper: func(ctx context.Context) (landBatchStepper, func(), error) {
			defer observeLandBatchPhase(landBatchPhaseQuarantine, time.Now())

			quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, repository)
			if err != nil {
				return nil, nil, err
			}

			objectHash, err := quarantineRepo.ObjectHash(ctx)
			if err != nil {
				cleanup()
				return nil, nil, structerr.NewInternal("detecting object hash: %w", err)
			}

			checker, releaseChecker, err := s.landAncestry(ctx, quarantineRepo)
			if err != nil {
				cleanup()
				return nil, nil, err
			}

			// One rebase engine serves the whole attempt, and the attempt ends with it.
			rebaser := newBatchRebaser(quarantineRepo)

			return &serverLandBatchStepper{
					server:        s,
					repository:    repository,
					quarantineDir: quarantineDir,
					repo:          quarantineRepo,
					objectHash:    objectHash,
					rebaser:       rebaser,
					ancestry:      checker,
				}, func() {
					releaseChecker()
					if err := rebaser.Close(); err != nil {
						s.logger.WithError(err).Error("graft: closing the rebaser of a land batch")
					}
					cleanup()
				}, nil
		},
		fallback: func(req *landBatchRequest) (*gitalypb.UserLandResponse, error) {
			return s.landWithRetries(req.ctx, req.in, req.committer)
		},
		giveUp: landBatchGiveUp,
		backoff: func(ctx context.Context, attempt int) error {
			if err := waitLandBackoff(ctx, attempt); err != nil {
				return err
			}
			// In authoritative storage mode the branch may have moved on another node: the
			// rebuilt batch must start from the tip that the database has.
			return s.ensureStorageFresh(ctx, repository)
		},
		early: func(ctx context.Context, index int, outcome landBatchOutcome) {
			landlane.CompleteEarly(ctx, index, landBatchResult(outcome))
		},
	}

	// The lane context carries the values of the first request but no cancellation. Git commands
	// need a context that can be done, so the batch gets one of its own that ends with the batch,
	// or when the graceful shutdown period of the server has elapsed: the lanes wait for their
	// running batches when they are closed at shutdown.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.shutdownCtx, cancel)()

	outcomes := executor.Execute(ctx, reqs)
	countLandBatchMembers(outcomes)

	results := make([]landlane.Result[*gitalypb.UserLandResponse], len(outcomes))
	for i, outcome := range outcomes {
		results[i] = landBatchResult(outcome)
	}

	return results, nil
}

// landBatchResult builds the result of a request of a batch.
func landBatchResult(outcome landBatchOutcome) landlane.Result[*gitalypb.UserLandResponse] {
	if outcome.Err != nil {
		return landlane.Result[*gitalypb.UserLandResponse]{Err: outcome.Err}
	}

	if outcome.Response != nil {
		// A request landed alone after its batch was rejected. It did not share a batch.
		outcome.Response.BatchSize = 1
		outcome.Response.BatchPosition = 1
		return landlane.Result[*gitalypb.UserLandResponse]{Value: outcome.Response}
	}

	resp := landResponse(outcome.OldTip, outcome.LandedCommit, outcome.Rewritten, outcome.AlreadyLanded, outcome.Created)
	resp.Attempts = int32(outcome.Attempts)
	resp.BatchSize = int32(outcome.BatchSize)
	resp.BatchPosition = int32(outcome.BatchPosition + 1)
	if outcome.Resolution != nil {
		resp.ResolvedPaths = outcome.Resolution.resolvedPaths
		resp.Resolver = outcome.Resolution.resolver
	}

	return landlane.Result[*gitalypb.UserLandResponse]{Value: resp}
}

// landBatchGiveUp returns the error of a request whose batch lost the compare-and-swap on every
// attempt.
func landBatchGiveUp(req *landBatchRequest, cause error, attempts int) error {
	var conflict landConflict
	if !errors.As(cause, &conflict) {
		return structerr.NewInternal("updating ref with hooks: %w", cause)
	}

	return landKeptMovingError(req.in, conflict, attempts)
}

// batchRebaser rebases the commits of a batch attempt. It has the semantics of localrepo.Rebase, and
// is rebase.Engine.
type batchRebaser interface {
	Rebase(ctx context.Context, upstream, branch git.ObjectID, opts ...localrepo.RebaseOption) (git.ObjectID, error)
	Close() error
}

// newBatchRebaser starts the rebaser of a batch attempt on the quarantine of the attempt. It is a
// variable so that tests can observe the rebases.
var newBatchRebaser = func(repo *localrepo.Repo) batchRebaser {
	return rebase.New(repo)
}

// serverLandBatchStepper implements landBatchStepper on the quarantine of one batch attempt.
type serverLandBatchStepper struct {
	server        *Server
	repository    *gitalypb.Repository
	quarantineDir *quarantine.Dir
	repo          *localrepo.Repo
	objectHash    git.ObjectHash
	// rebaser rebases the members of the attempt. It serves the whole attempt.
	rebaser batchRebaser
	// ancestry answers the ancestry questions of the attempt, with the graph index when it is enabled.
	ancestry ancestry.Checker
}

// Tip returns the tip of the reference. A missing reference yields an empty ID.
func (st *serverLandBatchStepper) Tip(ctx context.Context, reference git.ReferenceName) (git.ObjectID, error) {
	defer observeLandBatchPhase(landBatchPhaseTip, time.Now())

	tip, err := st.repo.ResolveRevision(ctx, reference.Revision())
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return "", nil
		}

		return "", structerr.NewInternal("resolving branch: %w", err)
	}

	return tip, nil
}

// Land lands the request onto tip. It mirrors the decision of landAttempt for a single request. The
// standalone policy is evaluated for the actor on the new tip the request would create, before the
// request changes the running tip.
//
// The git work runs in the context of the batch, which a submitter does not cancel: the request is
// part of the batch from the moment it lands. The identity is the one of the request, not the one of
// the first request of the batch, because conflict resolution is decided by it.
func (st *serverLandBatchStepper) Land(ctx context.Context, tip git.ObjectID, req *landBatchRequest) (landStep, error) {
	defer observeLandBatchPhase(landBatchPhaseLandMember, time.Now())

	ctx = requestIdentityContext(ctx, req)
	in := req.in
	reference := req.reference()

	commitID, err := st.objectHash.FromHex(in.GetCommitId())
	if err != nil {
		return landStep{tipIndependent: true}, structerr.NewInvalidArgument("cannot parse commit ID: %w", err).
			WithMetadata("commit_id", in.GetCommitId())
	}

	commitID, err = st.repo.ResolveRevision(ctx, git.Revision(commitID.String()+"^{commit}"))
	if err != nil {
		return landStep{tipIndependent: true}, structerr.NewInvalidArgument("cannot resolve commit ID: %w", err).
			WithMetadata("commit_id", in.GetCommitId())
	}

	if tip == "" {
		if !in.GetCreateBranch() {
			return landStep{}, structerr.NewNotFound("branch %q does not exist", string(in.GetBranch()))
		}

		if err := st.admit(req, reference, st.objectHash.ZeroOID, commitID); err != nil {
			return landStep{}, err
		}

		return landStep{commit: commitID, tip: commitID, created: true}, nil
	}

	landed, err := st.ancestry.IsAncestor(ctx, commitID, tip)
	if err != nil {
		return landStep{}, structerr.NewInternal("checking whether commit is already landed: %w", err)
	}
	if landed {
		return landStep{commit: commitID, tip: tip, alreadyLanded: true}, nil
	}

	fastForward, err := st.ancestry.IsAncestor(ctx, tip, commitID)
	if err != nil {
		return landStep{}, structerr.NewInternal("checking for ancestry: %w", err)
	}
	if fastForward {
		if err := st.admit(req, reference, tip, commitID); err != nil {
			return landStep{}, err
		}

		return landStep{commit: commitID, tip: commitID}, nil
	}

	newTip, err := st.computeTip(ctx, in, tip, commitID, req.committer)
	var resolution *landResolution
	if err != nil {
		if !in.GetResolveConflicts() || !isLandMergeConflict(err) {
			return landStep{}, err
		}

		res, resolveErr := st.server.resolveLandConflicts(ctx, in, st.repo, tip, commitID, req.committer)
		if resolveErr != nil {
			st.server.logger.WithError(resolveErr).WithField("branch", string(in.GetBranch())).
				Info("graft: conflict resolution of land failed")
			return landStep{}, err
		}

		resolution = &res
		newTip = res.commit
	}

	// The rebase dropped the change because the tip already holds all of it. The request reports
	// a landing that changed nothing, as landAttempt does.
	if resolution == nil && newTip == tip {
		return landStep{commit: commitID, tip: tip}, nil
	}

	if err := st.admit(req, reference, tip, newTip); err != nil {
		return landStep{}, err
	}

	return landStep{commit: commitID, tip: newTip, rewritten: true, resolution: resolution}, nil
}

// computeTip computes the new tip of the attempt for a commit that is neither reachable from the tip
// nor a fast-forward of it. It is computeLandedTip, except that the rebase strategy rebases with the
// rebaser of the attempt and checks the common ancestor with the ancestry checker of the attempt.
// The rebase has the semantics and the errors of the rebase of computeLandedTip.
//
// The single-land path keeps its own computeLandedTip (land.go is not changed by the batch executor), so
// the rebase branch below mirrors it.
func (st *serverLandBatchStepper) computeTip(ctx context.Context, in *gitalypb.UserLandRequest, tip, commitID git.ObjectID, committer git.Signature) (git.ObjectID, error) {
	if in.GetStrategy() != gitalypb.UserLandRequest_STRATEGY_REBASE_UNSPECIFIED {
		return st.server.computeLandedTip(ctx, in, st.repo, tip, commitID, committer)
	}

	if err := rejectMergeCommitsInRange(ctx, st.repo, tip, commitID); err != nil {
		return "", err
	}

	// Rebasing needs a merge base. Unrelated histories can only be landed with STRATEGY_MERGE.
	_, related, err := st.ancestry.MergeBase(ctx, tip, commitID)
	if err != nil {
		return "", structerr.NewInternal("checking for a common ancestor: %w", err)
	}
	if !related {
		return "", structerr.NewInvalidArgument("commit shares no history with branch %q: use STRATEGY_MERGE to land unrelated history", string(in.GetBranch()))
	}

	// Reapplied cherry-picks as in computeLandedTip: see the comment there.
	rebasedCommitID, err := st.rebaser.Rebase(ctx, tip, commitID,
		localrepo.RebaseWithCommitter(committer),
		localrepo.RebaseWithReappliedCherryPicks(),
	)
	if err != nil {
		// The conflict error is passed as the cause itself, see computeLandedTip.
		var conflictErr *localrepo.RebaseConflictError
		if errors.As(err, &conflictErr) {
			return "", landMergeConflictError(tip, commitID, conflictingFileNames(conflictErr.ConflictError), conflictErr)
		}

		return "", structerr.NewInternal("rebasing commit: %w", err)
	}

	return rebasedCommitID, nil
}

// admit evaluates the standalone access policy for the actor of req on the update of reference from
// oldOID to newOID. The actor is the one the pre-receive hook would evaluate for the request alone: the
// agent of the request, or else its GitLab user ID. The combined update does not evaluate it, as it
// runs as the graft-batch user, so every member is checked here, before it enters the running tip.
// A denied request fails alone with the reason of the policy.
func (st *serverLandBatchStepper) admit(req *landBatchRequest, reference git.ReferenceName, oldOID, newOID git.ObjectID) error {
	allowed, message, err := hook.EvaluateGraftPolicy(st.server.graft, req.actor, reference.String(), oldOID.String(), newOID.String())
	if err != nil {
		return structerr.NewInternal("evaluating graft policy: %w", err)
	}

	if allowed {
		return nil
	}

	return structerr.NewPermissionDenied("%s", message).WithDetail(&gitalypb.UserLandError{
		Error: &gitalypb.UserLandError_AccessCheck{
			AccessCheck: &gitalypb.AccessCheckError{
				ErrorMessage: message,
				UserId:       req.actor,
			},
		},
	})
}

// Update publishes the batch with one compare-and-swap. The hooks run once for the combined update,
// as the synthetic graft-batch user, and the changelog records the members of the batch.
func (st *serverLandBatchStepper) Update(ctx context.Context, reference git.ReferenceName, members []changelog.Member, rewritten bool, old, newTip git.ObjectID) (landUpdateKind, error) {
	defer observeLandBatchPhase(landBatchPhaseUpdate, time.Now())

	oldOID := old
	if old == "" {
		oldOID = st.objectHash.ZeroOID
	}

	ctx = batchUpdateContext(ctx)
	ctx = changelog.WithBatch(changelog.WithOperation(ctx, changelog.OperationLandBatch), members)

	// The compare-and-swap reads only the repository and the user of the request.
	in := &gitalypb.UserLandRequest{
		Repository: st.repository,
		User:       landBatchIdentity,
	}

	return st.updateKind(st.server.updateLandedReference(ctx, in, st.quarantineDir, st.repo, reference, oldOID, newTip, rewritten))
}

// UpdateSolo publishes a batch of one member as a land: the update runs with the identity of the member
// and as the land operation. The changelog records the member as the changeset of the land, so the
// original commit of the member maps to the commit it landed as. The compare-and-swap and its outcomes
// are those of Update.
func (st *serverLandBatchStepper) UpdateSolo(ctx context.Context, req *landBatchRequest, member changelog.Member, reference git.ReferenceName, rewritten bool, old, newTip git.ObjectID) (landUpdateKind, error) {
	defer observeLandBatchPhase(landBatchPhaseUpdate, time.Now())

	oldOID := old
	if old == "" {
		oldOID = st.objectHash.ZeroOID
	}

	ctx = requestIdentityContext(ctx, req)
	ctx = changelog.WithBatch(changelog.WithOperation(ctx, changelogOperationLand), []changelog.Member{member})

	return st.updateKind(st.server.updateLandedReference(ctx, req.in, st.quarantineDir, st.repo, reference, oldOID, newTip, rewritten))
}

// changelogOperationLand is the operation of an unbatched land in the changelog. The lanes run outside
// of the gRPC method of the requests, so the operation is set for the update of a batch of one.
const changelogOperationLand = "land"

// updateKind classifies the error of the compare-and-swap of a batch or of a batch of one.
func (st *serverLandBatchStepper) updateKind(err error) (landUpdateKind, error) {
	if err == nil {
		return landUpdated, nil
	}

	var conflict landConflict
	if errors.As(err, &conflict) {
		return landUpdateLost, err
	}

	var customHookErr updateref.CustomHookError
	var notAllowedErr hook.NotAllowedError
	if errors.As(err, &customHookErr) || errors.As(err, &notAllowedErr) {
		return landUpdateRejected, err
	}

	return landUpdateFailed, err
}
