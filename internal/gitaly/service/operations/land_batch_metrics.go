package operations

import (
	"time"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The phases of a land batch that graft_land_batch_phase_seconds observes.
const (
	// landBatchPhaseQuarantine is the creation of the quarantine of a batch attempt.
	landBatchPhaseQuarantine = "quarantine"
	// landBatchPhaseTip is the reading of the tip of the branch by a batch attempt.
	landBatchPhaseTip = "tip"
	// landBatchPhaseLandMember is the landing of one request onto the running tip. It is observed
	// once per request of an attempt.
	landBatchPhaseLandMember = "land_member"
	// landBatchPhaseUpdate is the compare-and-swap that publishes a batch, with the hooks.
	landBatchPhaseUpdate = "update"
	// landBatchPhaseTotal is the whole execution of a batch, from its start to its outcomes.
	landBatchPhaseTotal = "total"
)

// The outcomes of a request that graft_land_batch_members_total counts.
const (
	// landBatchOutcomeLanded is a request whose change is on the branch.
	landBatchOutcomeLanded = "landed"
	// landBatchOutcomeAlreadyLanded is a request whose commit was already on the branch.
	landBatchOutcomeAlreadyLanded = "already_landed"
	// landBatchOutcomeConflict is a request that failed because its change conflicts with the branch,
	// or because the branch kept moving until the request ran out of attempts.
	landBatchOutcomeConflict = "conflict"
	// landBatchOutcomeDenied is a request that the access policy or the hooks denied.
	landBatchOutcomeDenied = "denied"
	// landBatchOutcomeError is a request that failed for any other reason.
	landBatchOutcomeError = "error"
)

var (
	landBatchPhaseSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "graft_land_batch_phase_seconds",
		Help: "Time spent in each phase of a land batch: quarantine, tip, land_member (one observation per " +
			"request of a batch attempt), update (the compare-and-swap with the hooks) and total.",
		Buckets: prometheus.DefBuckets,
	}, []string{"phase"})

	landBatchMembersTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "graft_land_batch_members_total",
		Help: "Land requests executed by batches, by outcome: landed, already_landed, conflict, denied or error.",
	}, []string{"outcome"})
)

// observeLandBatchPhase observes the time elapsed since start in phase. It is meant for defer, which
// evaluates start when the deferred statement runs:
//
//	defer observeLandBatchPhase(landBatchPhaseTip, time.Now())
func observeLandBatchPhase(phase string, start time.Time) {
	landBatchPhaseSeconds.WithLabelValues(phase).Observe(time.Since(start).Seconds())
}

// countLandBatchMembers counts the outcome of every request of an executed batch.
func countLandBatchMembers(outcomes []landBatchOutcome) {
	for _, outcome := range outcomes {
		landBatchMembersTotal.WithLabelValues(landBatchOutcomeLabel(outcome)).Inc()
	}
}

// landBatchOutcomeLabel returns the outcome label of a request of a batch.
func landBatchOutcomeLabel(outcome landBatchOutcome) string {
	if outcome.Err != nil {
		return landBatchErrorLabel(outcome.Err)
	}

	if outcome.Response != nil {
		// A request that landed alone after its batch was rejected.
		if outcome.Response.GetAlreadyLanded() {
			return landBatchOutcomeAlreadyLanded
		}
		return landBatchOutcomeLanded
	}

	if outcome.AlreadyLanded {
		return landBatchOutcomeAlreadyLanded
	}

	return landBatchOutcomeLanded
}

// landBatchErrorLabel returns the outcome label of a request that failed with err.
func landBatchErrorLabel(err error) string {
	st := status.Convert(err)
	if st.Code() == codes.PermissionDenied {
		return landBatchOutcomeDenied
	}

	for _, detail := range st.Details() {
		landErr, ok := detail.(*gitalypb.UserLandError)
		if ok && (landErr.GetMergeConflict() != nil || landErr.GetReferenceUpdate() != nil) {
			return landBatchOutcomeConflict
		}
	}

	return landBatchOutcomeError
}
