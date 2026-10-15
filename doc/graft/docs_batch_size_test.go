package graft

import (
	"strings"
	"testing"
)

// TestBatchSizeDocsMatchExecutor is the regression test for docs that said batch_size and
// batch_position count only the requests that landed. The executor sets BatchSize to the number of
// requests of the batch and BatchPosition to the submission index of the request, for every
// response without a result (internal/gitaly/service/operations/land_batch.go, Execute), and the
// early answers use the same values (deliverEarly). The response sends the position 1-based.
func TestBatchSizeDocsMatchExecutor(t *testing.T) {
	const file = "LAND-BATCHING.md"
	text := strings.Join(strings.Fields(specDoc(t, file)), " ")

	for _, stale := range []string{
		"batch_size` counts the requests that landed",
		"Requests that failed are not counted",
		"count only them",
		"among those requests",
	} {
		if strings.Contains(text, stale) {
			t.Errorf("%s still describes batch_size or batch_position as counting only landed requests: %q", file, stale)
		}
	}

	for _, want := range []string{
		"Number of requests of the batch as submitted",
		"`batch_size` is the number of requests of the batch",
		"Failed and early-answered requests are counted too",
		"still describe the batch as submitted",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s must say that batch_size counts every request of the batch: missing %q", file, want)
		}
	}
}

// TestMetricDocsDescribeObservations is the regression test for the phase metric descriptions:
// total is observed once per execution of the batch, and land_member once per request the stepper
// lands in every attempt, not only per landed request.
func TestMetricDocsDescribeObservations(t *testing.T) {
	const file = "LAND-BATCHING.md"
	text := strings.Join(strings.Fields(specDoc(t, file)), " ")

	for _, stale := range []string{
		"(the whole attempt)",
		"(one observation per request landed on the running tip)",
	} {
		if strings.Contains(text, stale) {
			t.Errorf("%s still describes the phase metric wrongly: %q", file, stale)
		}
	}
	if !strings.Contains(text, "one observation per execution of the batch") {
		t.Errorf("%s must say that the total phase is observed once per execution of the batch", file)
	}
}

// TestGraphAncestryDocsDoNotNameCommitGraph is the regression test for the graph_ancestry docs:
// ancestry reads the graph kind, and does not depend on the commit-graph file or on commit_graph.
func TestGraphAncestryDocsDoNotNameCommitGraph(t *testing.T) {
	text := strings.Join(strings.Fields(specDoc(t, "CONFIGURATION.md")), " ")
	if strings.Contains(text, "stored commit-graph index") {
		t.Error("CONFIGURATION.md must not say that graph_ancestry reads the commit-graph index")
	}
	if !strings.Contains(text, "stored `graph` index") {
		t.Error("CONFIGURATION.md must say that graph_ancestry reads the stored graph index")
	}

	derived := strings.Join(strings.Fields(specDoc(t, "DERIVED.md")), " ")
	if strings.Contains(derived, "including a plain Gitaly") {
		t.Error("DERIVED.md must say a non-standalone Gitaly, not a plain Gitaly with a [graft] section")
	}
}
