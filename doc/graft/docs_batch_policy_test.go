package graft

import (
	"strings"
	"testing"
)

// TestBatchPolicyDocsMatchHooks is the regression test for docs that said a
// policy rule matching graft-batch rejects the combined update of a batch. The
// pre-receive hook skips its access check, and the reference-transaction hook
// its policy check, for an update with operation land_batch and members
// (internal/gitaly/hook/prereceive.go, referencetransaction.go), so such a rule
// has no effect.
func TestBatchPolicyDocsMatchHooks(t *testing.T) {
	for _, file := range []string{"LAND-BATCHING.md", "LIMITATIONS.md"} {
		text := strings.Join(strings.Fields(specDoc(t, file)), " ")
		for _, stale := range []string{
			"policy rule that matches `graft-batch`, makes",
			"the policy as applied to the `graft-batch` user rejected",
		} {
			if strings.Contains(text, stale) {
				t.Errorf("%s still says that the policy can reject a batch: %q", file, stale)
			}
		}
		if !strings.Contains(text, "has no effect") {
			t.Errorf("%s must say that a policy rule for graft-batch has no effect", file)
		}
	}

	text := strings.Join(strings.Fields(specDoc(t, "LAND-BATCHING.md")), " ")
	if !strings.Contains(text, "access check of the pre-receive hook") {
		t.Error("LAND-BATCHING.md must say that the pre-receive access check is skipped for the combined update")
	}
}
