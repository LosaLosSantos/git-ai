package operations

import (
	"errors"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// landBatchMetricCounts returns the sample counts of the phases of graft_land_batch_phase_seconds and
// the values of the outcomes of graft_land_batch_members_total.
func landBatchMetricCounts(t *testing.T) (map[string]uint64, map[string]float64) {
	t.Helper()

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(landBatchPhaseSeconds))
	require.NoError(t, reg.Register(landBatchMembersTotal))

	families, err := reg.Gather()
	require.NoError(t, err)

	phases := map[string]uint64{}
	outcomes := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			label := metric.GetLabel()[0].GetValue()
			switch family.GetName() {
			case "graft_land_batch_phase_seconds":
				phases[label] = metric.GetHistogram().GetSampleCount()
			case "graft_land_batch_members_total":
				outcomes[label] = metric.GetCounter().GetValue()
			}
		}
	}

	return phases, outcomes
}

// TestLandBatchMetricsObservePhases is not parallel: the metrics are process-wide, so the difference
// of their values is only this batch when no other batch runs meanwhile.
func TestLandBatchMetricsObservePhases(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	ctx, cfg, client := setupLandBatchService(t, ctx, batchingGraft())
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(baseEntry),
	)

	changes := make([]git.ObjectID, 0, 3)
	for _, name := range []string{"a", "b", "c"} {
		changes = append(changes, gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithParents(base),
			gittest.WithMessage("land "+name),
			gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: name, Content: name}),
		))
	}

	phasesBefore, outcomesBefore := landBatchMetricCounts(t)

	responses, errs := landAll(ctx, client, repoProto, changes)
	for i := range changes {
		require.NoError(t, errs[i])
		require.NotEmpty(t, responses[i].GetLandedCommitId())
	}

	phasesAfter, outcomesAfter := landBatchMetricCounts(t)

	for _, phase := range []string{
		landBatchPhaseQuarantine,
		landBatchPhaseTip,
		landBatchPhaseLandMember,
		landBatchPhaseUpdate,
		landBatchPhaseTotal,
	} {
		require.Greater(t, phasesAfter[phase], phasesBefore[phase], "phase %q must be observed", phase)
	}

	// One observation per member of the batch attempt, at least one attempt for three members.
	require.GreaterOrEqual(t, phasesAfter[landBatchPhaseLandMember]-phasesBefore[landBatchPhaseLandMember], uint64(3))
	require.Equal(t, 3.0, outcomesAfter[landBatchOutcomeLanded]-outcomesBefore[landBatchOutcomeLanded])
}

func TestLandBatchOutcomeLabel(t *testing.T) {
	t.Parallel()

	const zero = git.ObjectID("0000000000000000000000000000000000000000")
	const commit = git.ObjectID("1111111111111111111111111111111111111111")

	mergeConflict := landMergeConflictError(zero, commit, [][]byte{[]byte("a")}, errors.New("conflicting files"))
	keptMoving := structerr.NewFailedPrecondition("kept moving").WithDetail(
		landReferenceUpdateError(git.ReferenceName("refs/heads/main"), zero, commit),
	)

	for _, tc := range []struct {
		desc    string
		outcome landBatchOutcome
		label   string
	}{
		{desc: "landed", outcome: landBatchOutcome{LandedCommit: commit}, label: landBatchOutcomeLanded},
		{desc: "already landed", outcome: landBatchOutcome{AlreadyLanded: true}, label: landBatchOutcomeAlreadyLanded},
		{
			desc:    "landed alone",
			outcome: landBatchOutcome{Response: &gitalypb.UserLandResponse{}},
			label:   landBatchOutcomeLanded,
		},
		{
			desc:    "already landed alone",
			outcome: landBatchOutcome{Response: &gitalypb.UserLandResponse{AlreadyLanded: true}},
			label:   landBatchOutcomeAlreadyLanded,
		},
		{desc: "merge conflict", outcome: landBatchOutcome{Err: mergeConflict}, label: landBatchOutcomeConflict},
		{desc: "kept moving", outcome: landBatchOutcome{Err: keptMoving}, label: landBatchOutcomeConflict},
		{
			desc:    "denied",
			outcome: landBatchOutcome{Err: structerr.NewPermissionDenied("denied")},
			label:   landBatchOutcomeDenied,
		},
		{
			desc:    "other error",
			outcome: landBatchOutcome{Err: status.Error(codes.Internal, "boom")},
			label:   landBatchOutcomeError,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.label, landBatchOutcomeLabel(tc.outcome))
		})
	}
}
