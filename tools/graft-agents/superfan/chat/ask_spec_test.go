package chat

import (
	"os/exec"
	"testing"
)

// Spec spec: `graft-superfan chat ask "question"` uses the same engine as the
// server. With provider "none" the answer is a deterministic summary of the
// sources, so two runs with the same question must print the same non-empty
// output. ASSUMPTION: --config is accepted by `chat ask` (not stated in spec).
func TestSpecAskNoneProviderIsDeterministic(t *testing.T) {
	bin := specBuild(t)
	cfg := specConfig(t)
	const question = "how is the component doing?"

	run := func() string {
		cmd := exec.Command(bin, "chat", "ask", "--config", cfg, question)
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				t.Fatalf("chat ask failed: %v\nstderr: %s", err, ee.Stderr)
			}
			t.Fatalf("chat ask failed: %v", err)
		}
		return string(out)
	}

	first := run()
	if first == "" {
		t.Fatal("chat ask with provider none printed nothing")
	}
	second := run()
	if first != second {
		t.Errorf("provider none is not deterministic:\nfirst:  %q\nsecond: %q", first, second)
	}
}
