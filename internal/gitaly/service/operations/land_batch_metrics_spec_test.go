package operations

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// Spec (PLAN4 WP31):
//   Histogram graft_land_batch_phase_seconds{phase} with phases
//   quarantine, tip, land_member (one observation per member), update, total.
//   Counter graft_land_batch_members_total{outcome} with outcomes
//   landed, already_landed, conflict, denied, error.
//   Metrics are registered on the same registry as the other graft metrics;
//   these tests read them from prometheus.DefaultGatherer.

const (
	specWP31PhaseMetric   = "graft_land_batch_phase_seconds"
	specWP31MembersMetric = "graft_land_batch_members_total"
)

var specWP31Phases = []string{"quarantine", "tip", "land_member", "update", "total"}

var specWP31Outcomes = []string{"landed", "already_landed", "conflict", "denied", "error"}

func specWP31Family(t *testing.T, name string) *dto.MetricFamily {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() == name {
			return family
		}
	}

	return nil
}

func specWP31LabelValue(metric *dto.Metric, label string) string {
	for _, pair := range metric.GetLabel() {
		if pair.GetName() == label {
			return pair.GetValue()
		}
	}

	return ""
}

// specWP31PhaseCount returns the number of observations of one phase, 0 if the
// phase has no series yet.
func specWP31PhaseCount(t *testing.T, phase string) uint64 {
	t.Helper()

	family := specWP31Family(t, specWP31PhaseMetric)
	if family == nil {
		return 0
	}

	for _, metric := range family.GetMetric() {
		if specWP31LabelValue(metric, "phase") == phase {
			return metric.GetHistogram().GetSampleCount()
		}
	}

	return 0
}

// specWP31PhaseSum returns the summed observed seconds of one phase.
func specWP31PhaseSum(t *testing.T, phase string) float64 {
	t.Helper()

	family := specWP31Family(t, specWP31PhaseMetric)
	if family == nil {
		return 0
	}

	for _, metric := range family.GetMetric() {
		if specWP31LabelValue(metric, "phase") == phase {
			return metric.GetHistogram().GetSampleSum()
		}
	}

	return 0
}

// specWP31OutcomeTotal returns the summed value of the members counter over
// all outcomes.
func specWP31OutcomeTotal(t *testing.T) float64 {
	t.Helper()

	family := specWP31Family(t, specWP31MembersMetric)
	if family == nil {
		return 0
	}

	var total float64
	for _, metric := range family.GetMetric() {
		total += metric.GetCounter().GetValue()
	}

	return total
}

// specWP31DriveThreeMemberBatch would run a 3-member batch through testserver.
// PLAN4 WP31 does not name the batch RPC or its request type, so the driver
// cannot be written from the spec alone. Until the batch entry point is named,
// the tests that need a batch skip.
func specWP31DriveThreeMemberBatch(t *testing.T) {
	t.Helper()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	_, _ = ctx, cfg

	t.Skip("PLAN4 WP31 does not name the batch RPC; driver not specified")
}

func TestSpecPhaseHistogramUsesSpecifiedPhases(t *testing.T) {
	family := specWP31Family(t, specWP31PhaseMetric)
	if family == nil {
		specWP31DriveThreeMemberBatch(t)
		t.Fatal("histogram has no series after a batch run")
	}

	require.Equal(t, dto.MetricType_HISTOGRAM, family.GetType())

	for _, metric := range family.GetMetric() {
		require.Contains(t, specWP31Phases, specWP31LabelValue(metric, "phase"))
	}
}

func TestSpecMembersCounterUsesSpecifiedOutcomes(t *testing.T) {
	family := specWP31Family(t, specWP31MembersMetric)
	if family == nil {
		specWP31DriveThreeMemberBatch(t)
		t.Fatal("counter has no series after a batch run")
	}

	require.Equal(t, dto.MetricType_COUNTER, family.GetType())

	for _, metric := range family.GetMetric() {
		require.Contains(t, specWP31Outcomes, specWP31LabelValue(metric, "outcome"))
	}
}

func TestSpecThreeMemberBatchObservesEachPhase(t *testing.T) {
	beforeMember := specWP31PhaseCount(t, "land_member")
	beforeQuarantine := specWP31PhaseCount(t, "quarantine")
	beforeTip := specWP31PhaseCount(t, "tip")
	beforeUpdate := specWP31PhaseCount(t, "update")
	beforeTotal := specWP31PhaseCount(t, "total")

	specWP31DriveThreeMemberBatch(t)

	// One land_member observation per member; the other phases once per batch.
	require.Equal(t, beforeMember+3, specWP31PhaseCount(t, "land_member"))
	require.Equal(t, beforeQuarantine+1, specWP31PhaseCount(t, "quarantine"))
	require.Equal(t, beforeTip+1, specWP31PhaseCount(t, "tip"))
	require.Equal(t, beforeUpdate+1, specWP31PhaseCount(t, "update"))
	require.Equal(t, beforeTotal+1, specWP31PhaseCount(t, "total"))
}

func TestSpecEachMemberCountsExactlyOneOutcome(t *testing.T) {
	beforeMember := specWP31PhaseCount(t, "land_member")
	beforeOutcomes := specWP31OutcomeTotal(t)

	specWP31DriveThreeMemberBatch(t)

	// Every member is one land_member observation and one outcome.
	require.Equal(t, beforeMember+3, specWP31PhaseCount(t, "land_member"))
	require.Equal(t, beforeOutcomes+3, specWP31OutcomeTotal(t))
}

func TestSpecTotalPhaseCoversMemberWork(t *testing.T) {
	beforeTotal := specWP31PhaseSum(t, "total")
	beforeMembers := specWP31PhaseSum(t, "land_member")

	specWP31DriveThreeMemberBatch(t)

	// The total phase spans the whole executeLandBatch, so its time must be at
	// least the time spent on the members within the same batch.
	totalDelta := specWP31PhaseSum(t, "total") - beforeTotal
	membersDelta := specWP31PhaseSum(t, "land_member") - beforeMembers
	require.GreaterOrEqual(t, totalDelta, membersDelta)
}
