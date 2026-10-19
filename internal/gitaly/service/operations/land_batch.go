package operations

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// landBatchMaxRebuilds is the number of times a batch is rebuilt on a new tip after it lost the
// compare-and-swap of its reference to a concurrent writer. The batch is built at most
// landBatchMaxRebuilds+1 times.
const landBatchMaxRebuilds = 10

// landBatchRequest is one UserLand request that is executed as part of a batch.
type landBatchRequest struct {
	// ctx is the context of the request. The batch runs in the context of the lane, so the identity
	// and the cancellation of one request are read from its own context.
	ctx       context.Context
	in        *gitalypb.UserLandRequest
	committer git.Signature
	// actor is the actor of the request, as landBatchActor returns it: the agent that submitted the
	// request, or its GitLab user ID. The standalone access policy is evaluated for the actor before the
	// request changes the running tip of the batch, and the changelog records it for the member.
	actor string
	// precheck is the check of the request's resolution, run before the request entered its lane.
	precheck *landPrecheck
	// preResolution holds the verdicts the server made on the request's conflicting files before
	// it entered its lane (PLAN10 15.8); nil unless the request asked for resolution.
	preResolution *landPreResolution
}

// reference returns the branch the request lands on.
func (r *landBatchRequest) reference() git.ReferenceName {
	return git.NewReferenceNameFromBranchName(string(r.in.GetBranch()))
}

// landUpdateKind classifies the outcome of the compare-and-swap that publishes a batch.
type landUpdateKind int

const (
	// landUpdateNotNeeded means that the batch did not change the branch, so there was nothing to
	// publish.
	landUpdateNotNeeded landUpdateKind = iota
	// landUpdated means that the branch now points to the batch tip.
	landUpdated
	// landUpdateLost means that the compare-and-swap lost a race against a concurrent writer. The
	// batch is rebuilt on the new tip.
	landUpdateLost
	// landUpdateRejected means that the hooks rejected the combined update. Each request is then
	// landed alone, so the rejection is attributed to the request that caused it.
	landUpdateRejected
	// landUpdateFailed means that the update failed for a reason that a retry does not fix.
	landUpdateFailed
)

// landStep is the result of landing one request onto the running tip of its batch.
type landStep struct {
	// commit is the commit the request asked to land, resolved from its commit ID.
	commit git.ObjectID
	// tip is the tip after the request. It equals the tip the request started from when the
	// request changed nothing, and the request's own landed commit otherwise.
	tip           git.ObjectID
	rewritten     bool
	alreadyLanded bool
	created       bool
	resolution    *landResolution
	// tipIndependent is set with an error that does not depend on the running tip: the commit ID is
	// unparsable or does not resolve to a commit. Such an error is final wherever the request is.
	tipIndependent bool
}

// landBatchStepper performs the git work of a batch attempt. An attempt uses a single stepper, so
// the objects it writes are shared by all requests of the attempt.
type landBatchStepper interface {
	// Tip returns the current tip of the reference. An empty ID means that the reference does not
	// exist.
	Tip(ctx context.Context, reference git.ReferenceName) (git.ObjectID, error)
	// Land lands the request onto tip. An error fails the request only; the batch continues without
	// it.
	Land(ctx context.Context, tip git.ObjectID, req *landBatchRequest) (landStep, error)
	// Update moves reference from old to newTip in one compare-and-swap, recording members as the
	// changes of the update. An empty old ID creates the reference.
	Update(ctx context.Context, reference git.ReferenceName, members []changelog.Member, rewritten bool, old, newTip git.ObjectID) (landUpdateKind, error)
}

// landSoloStepper is implemented by steppers that publish a batch of one member as a plain land. The
// update then runs with the identity of the member and as the land operation, recording the member: the
// changeset is the one of an unbatched land, with the real actor. Steppers that do not implement it
// publish every batch with Update.
type landSoloStepper interface {
	// UpdateSolo moves reference from old to newTip in one compare-and-swap for the single member req,
	// whose landing is member. The update records member as the changeset of the land. It has the
	// outcomes of Update.
	UpdateSolo(ctx context.Context, req *landBatchRequest, member changelog.Member, reference git.ReferenceName, rewritten bool, old, newTip git.ObjectID) (landUpdateKind, error)
}

// landBatchOutcome is the result of one request of a batch.
type landBatchOutcome struct {
	Err error
	// Response is set for a request that was landed alone after the batch was rejected. Its
	// outcome does not come from the batch.
	Response      *gitalypb.UserLandResponse
	OldTip        git.ObjectID
	LandedCommit  git.ObjectID
	Rewritten     bool
	AlreadyLanded bool
	Created       bool
	Resolution    *landResolution
	// BatchSize is the number of requests of the batch, the same for every response of the batch.
	BatchSize int
	// BatchPosition is the zero-based index of the request in the batch, in submission
	// order.
	BatchPosition int
	// Attempts is the number of batch attempts made before the outcome was reached.
	Attempts int
}

// landBatchExecutor lands batches of requests onto a single reference. Each request is landed in
// order onto the running tip, and the resulting tip is published with one compare-and-swap.
type landBatchExecutor struct {
	// maxRebuilds is the number of rebuilds after lost compare-and-swaps.
	maxRebuilds int
	// newStepper starts a batch attempt. The returned cleanup is called when the attempt ends.
	newStepper func(ctx context.Context) (landBatchStepper, func(), error)
	// fallback lands one request alone, with its own context and identity. It is used when the hooks
	// reject the combined update.
	fallback func(req *landBatchRequest) (*gitalypb.UserLandResponse, error)
	// giveUp converts the cause of the last lost compare-and-swap into the error of a request that
	// ran out of rebuilds.
	giveUp func(req *landBatchRequest, cause error, attempts int) error
	// backoff waits before the next attempt after a lost compare-and-swap.
	backoff func(ctx context.Context, attempt int) error
	// early delivers the outcome of the request at index of reqs before the batch ends, when the outcome
	// is final before the compare-and-swap. The batch still returns the same outcome for the request,
	// which the caller ignores. Nil means no early delivery.
	early func(ctx context.Context, index int, outcome landBatchOutcome)
}

// Execute lands the requests in order and returns one outcome per request, in the order of reqs.
// All requests must share the reference. A request whose context is done before the batch starts
// fails with a canceled error and does not land.
func (e landBatchExecutor) Execute(ctx context.Context, reqs []*landBatchRequest) []landBatchOutcome {
	outcomes := make([]landBatchOutcome, len(reqs))

	idx := make([]int, 0, len(reqs))
	for i, req := range reqs {
		if err := req.ctx.Err(); err != nil {
			outcomes[i].Err = structerr.NewCanceled("landing canceled before the batch: %w", err)
			continue
		}
		idx = append(idx, i)
	}

	if len(idx) > 0 {
		e.run(ctx, reqs, idx, outcomes)
	}

	// Every response of the batch reports the size of the batch, and the index of its request in the
	// batch, as the lane reports the requests it delivers early. A request landed alone after a
	// rejection is not part of the batch, so it keeps the size and the position of its own landing.
	for i := range outcomes {
		if outcomes[i].Response == nil {
			outcomes[i].BatchSize = len(reqs)
			outcomes[i].BatchPosition = i
		}
	}

	return outcomes
}

// run lands the requests idx as one batch and rebuilds it while the reference keeps moving. The
// outcomes are written at the indices of the requests.
//
// When the combined update is not published, the members did not reach the branch. The requests
// after the first member were landed onto the running tip that holds the members, so their
// outcomes do not hold for the branch either: a request that was already landed, or whose
// change became empty, may rely on a member's commits, and a request that conflicted may have
// conflicted with a member only. These requests never report the outcome of such an attempt.
func (e landBatchExecutor) run(ctx context.Context, reqs []*landBatchRequest, idx []int, outcomes []landBatchOutcome) {
	// final holds the requests delivered early. Their outcomes are final, so they are never landed again:
	// they are left out of the attempts that follow, and the fallback does not land them alone. Their
	// outcomes are restored at the end, as the delivered ones are the outcomes the submitters received.
	final := make(map[int]landBatchOutcome)
	defer func() {
		for i, outcome := range final {
			outcomes[i] = outcome
		}
	}()

	rebuilds := 0
	for attempt := 1; ; attempt++ {
		idx = withoutFinal(idx, final)
		if len(idx) == 0 {
			return
		}

		batch, members, kind, err := e.attempt(ctx, reqs, idx, attempt, final)
		if err != nil {
			failRequests(outcomes, idx, err, attempt)
			return
		}

		switch kind {
		case landUpdateNotNeeded, landUpdated:
			storeBatch(outcomes, idx, batch, attempt)
			return

		case landUpdateLost:
			if rebuilds >= e.maxRebuilds {
				// Every request from the first member on depends on the batch that never reached
				// the branch, so all of them ran out of rebuilds. The requests before it landed
				// onto the published tip and keep their outcomes.
				cause := batch[members[0]].Err
				for pos := members[0]; pos < len(batch); pos++ {
					if _, ok := final[idx[pos]]; ok {
						continue
					}

					batch[pos] = landBatchOutcome{Err: e.giveUp(reqs[idx[pos]], cause, attempt)}
				}
				storeBatch(outcomes, idx, batch, attempt)
				return
			}
			rebuilds++

			if err := e.backoff(ctx, attempt); err != nil {
				failRequests(outcomes, idx, structerr.NewCanceled("waiting to rebuild landing batch: %w", err), attempt)
				return
			}

		case landUpdateRejected:
			// The rejection belongs to one of the requests, which is not known from the combined update.
			// Every request that is not final lands alone, so that the hooks judge each of them on its own.
			for _, i := range idx {
				if _, ok := final[i]; ok {
					continue
				}

				resp, err := e.fallback(reqs[i])
				outcomes[i] = landBatchOutcome{Response: resp, Err: err}
			}
			return

		case landUpdateFailed:
			// The members fail with the error of the update, which a rebuild does not fix. The
			// requests after the first member that are not members are landed again in a new
			// attempt, without the members, so that their outcomes come from the real tip. Each
			// such attempt drops at least one member, so this ends.
			kept, keptBatch, dependents := splitDependents(idx, batch, members)
			storeBatch(outcomes, kept, keptBatch, attempt)
			if len(dependents) == 0 {
				return
			}

			idx = dependents
		}
	}
}

// withoutFinal returns the requests of idx whose outcome is not final yet.
func withoutFinal(idx []int, final map[int]landBatchOutcome) []int {
	pending := make([]int, 0, len(idx))
	for _, i := range idx {
		if _, ok := final[i]; !ok {
			pending = append(pending, i)
		}
	}

	return pending
}

// splitDependents splits the requests idx of an attempt whose combined update was not published.
// dependents are the requests after the first member that are not members themselves: they were
// landed onto members that did not reach the branch. kept are the other requests, with their
// outcomes keptBatch. members must not be empty.
func splitDependents(idx []int, batch []landBatchOutcome, members []int) (kept []int, keptBatch []landBatchOutcome, dependents []int) {
	isMember := make(map[int]bool, len(members))
	for _, pos := range members {
		isMember[pos] = true
	}

	for pos, i := range idx {
		if pos > members[0] && !isMember[pos] {
			dependents = append(dependents, i)
			continue
		}

		kept = append(kept, i)
		keptBatch = append(keptBatch, batch[pos])
	}

	return kept, keptBatch, dependents
}

// attempt lands the requests idx onto the current tip of their reference and publishes the result.
// The returned outcomes are aligned with idx. members are the positions in idx of the requests that
// changed the running tip. The error is set only when the attempt could not start. attempt is the
// number of the attempt, which the outcomes report. A request whose outcome is final before the
// compare-and-swap is delivered as soon as it is decided, and recorded in final (see deliverEarly).
func (e landBatchExecutor) attempt(ctx context.Context, reqs []*landBatchRequest, idx []int, attempt int, final map[int]landBatchOutcome) ([]landBatchOutcome, []int, landUpdateKind, error) {
	stepper, cleanup, err := e.newStepper(ctx)
	if err != nil {
		return nil, nil, landUpdateFailed, err
	}
	defer cleanup()

	reference := reqs[idx[0]].reference()
	tip, err := stepper.Tip(ctx, reference)
	if err != nil {
		return nil, nil, landUpdateFailed, err
	}

	outs := make([]landBatchOutcome, len(idx))
	cur := tip
	rewritten := false
	var members []int
	var changes []changelog.Member
	for pos, i := range idx {
		req := reqs[i]
		outs[pos].OldTip = cur
		outs[pos].Attempts = attempt

		step, err := stepper.Land(ctx, cur, req)
		if err != nil {
			outs[pos].Err = err
			if landOutcomeFinalBeforeUpdate(err, step.tipIndependent, len(members) == 0) {
				e.deliverEarly(ctx, i, len(reqs), outs[pos], final)
			}
			continue
		}

		switch {
		case step.alreadyLanded:
			outs[pos].LandedCommit = cur
			outs[pos].AlreadyLanded = true

			// The answer depends on the tip only while no member precedes the request. A member
			// that does not reach the branch would take the answer away.
			if len(members) == 0 {
				e.deliverEarly(ctx, i, len(reqs), outs[pos], final)
			}

		case step.tip == cur:
			// The commit became empty on top of the running tip and was dropped. The request
			// reports a landing that changed nothing.
			outs[pos].LandedCommit = cur

		default:
			cur = step.tip
			rewritten = rewritten || step.rewritten

			members = append(members, pos)
			changes = append(changes, memberOf(req, step))

			outs[pos].LandedCommit = step.tip
			outs[pos].Rewritten = step.rewritten
			outs[pos].Created = step.created
			outs[pos].Resolution = step.resolution
		}
	}

	if cur == tip {
		return outs, nil, landUpdateNotNeeded, nil
	}

	var kind landUpdateKind
	if solo, ok := stepper.(landSoloStepper); ok && len(members) == 1 {
		kind, err = solo.UpdateSolo(ctx, reqs[idx[members[0]]], changes[0], reference, rewritten, tip, cur)
	} else {
		kind, err = stepper.Update(ctx, reference, changes, rewritten, tip, cur)
	}
	if err != nil {
		for _, pos := range members {
			outs[pos].Err = err
		}
	}

	return outs, members, kind, nil
}

// landOutcomeFinalBeforeUpdate reports whether the error of a landing is final before the
// compare-and-swap: the request is invalid, the policy denies it, or its change conflicts with the
// running tip and is not resolved. The outcome of such a request is reported without waiting for the
// update of its batch.
//
// The policy depends only on the actor and the reference, so a denial is final wherever the request
// is in the batch. An invalid commit ID does not depend on the tip, so it is final as well. A conflict
// and the other invalid requests depend on the running tip, which a member that does not reach the
// branch would change. They are final only when noMember is set, that is when no
// member precedes the request, as the outcome of an already landed request is.
func landOutcomeFinalBeforeUpdate(err error, tipIndependent, noMember bool) bool {
	if status.Code(err) == codes.PermissionDenied {
		return true
	}

	// An invalid commit ID does not depend on the tip, so it is final wherever the request is.
	if tipIndependent {
		return true
	}

	if !noMember {
		return false
	}

	if status.Code(err) == codes.InvalidArgument {
		return true
	}

	return isLandMergeConflict(err)
}

// deliverEarly delivers the outcome of the request at index i of the batch before the batch ends, when
// the executor has an early delivery, and records it in final. The request is not landed again after
// that: see run.
func (e landBatchExecutor) deliverEarly(ctx context.Context, i, size int, outcome landBatchOutcome, final map[int]landBatchOutcome) {
	if e.early == nil {
		return
	}

	// The response of an early request reports the batch as the other responses of the batch do: the
	// size of the batch and the position of the request in it (see Execute).
	outcome.BatchSize = size
	outcome.BatchPosition = i
	final[i] = outcome
	e.early(ctx, i, outcome)
}

// memberOf describes the landing of req for the changelog of the batch.
func memberOf(req *landBatchRequest, step landStep) changelog.Member {
	member := changelog.Member{
		OriginalCommit: step.commit.String(),
		Commit:         step.tip.String(),
		Actor:          req.actor,
		Rewritten:      step.rewritten,
	}

	if step.resolution != nil {
		for _, path := range step.resolution.resolvedPaths {
			member.ResolvedPaths = append(member.ResolvedPaths, string(path))
		}
	}

	return member
}

// storeBatch stores the outcomes batch of the requests idx of an attempt. The size and the position of
// the requests in the batch are set by Execute, so that they are the same for every response.
func storeBatch(outcomes []landBatchOutcome, idx []int, batch []landBatchOutcome, attempt int) {
	for pos, i := range idx {
		outcomes[i] = batch[pos]
		outcomes[i].Attempts = attempt
	}
}

// failRequests fails every request of idx with err.
func failRequests(outcomes []landBatchOutcome, idx []int, err error, attempt int) {
	for _, i := range idx {
		outcomes[i] = landBatchOutcome{Err: err, Attempts: attempt}
	}
}

// landBatchIdentity is the identity of the combined update of a batch. The update is one change
// of the reference on behalf of all requests of the batch, so the hooks see this user. The
// members are recorded in the changelog with their own actors.
var landBatchIdentity = &gitalypb.User{
	GlId:       "graft-batch",
	GlUsername: "graft-batch",
	Name:       []byte("Graft"),
	Email:      []byte("graft@localhost"),
}

// landBatchActor returns the actor that the standalone policy evaluates for a request when it lands
// alone: the agent that submitted it, or else the GitLab user ID of the request. The pre-receive hook
// evaluates the agent of an update that carries one, and the user ID of any other update.
func landBatchActor(ctx context.Context, in *gitalypb.UserLandRequest) string {
	if id, ok := identity.FromContext(ctx); ok && id.Source == identity.SourceAgent {
		return id.Name
	}

	return in.GetUser().GetGlId()
}

// requestIdentityContext returns ctx carrying the identity of the request, and no other identity. The
// batch runs in the context of the lane, whose identity belongs to the first request of the batch, so
// the identity of every request is taken from its own context. A request without identity clears it.
func requestIdentityContext(ctx context.Context, req *landBatchRequest) context.Context {
	id, _ := identity.FromContext(req.ctx)
	return identity.NewContext(ctx, id)
}

// batchUpdateContext returns ctx without any identity. The combined update is made by the batch, not by
// one of its requests: the hooks judge it as the graft-batch user and not as an agent, so the policy of
// the first request does not decide for the others. The members are recorded in the changelog with their
// own actors, and the policy of each member is checked before it lands (see admit).
func batchUpdateContext(ctx context.Context) context.Context {
	return identity.NewContext(ctx, identity.Identity{})
}
