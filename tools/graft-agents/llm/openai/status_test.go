package openai

import (
	"context"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// TestErrorStatusIsReported: the provider does not stream, so an error status is a refusal before
// generation; it is unbilled for 400 to 403, and not for a 5xx that outlasted the retries.
func TestErrorStatusIsReported(t *testing.T) {
	for _, tc := range []struct {
		status   int
		unbilled bool
	}{{400, true}, {401, true}, {402, true}, {403, true}, {404, false}, {429, false}, {500, false}} {
		f := newFakeServer(t, scripted(reply{status: tc.status, body: `{"error":{"message":"no"}}`}))
		p := newProvider(t, f)
		_, err := p.Complete(context.Background(), llm.Request{Prompt: "x"})
		if status, ok := llm.HTTPStatus(err); !ok || status != tc.status {
			t.Fatalf("%d: status %d (%v): %v", tc.status, status, ok, err)
		}
		if got := llm.Unbilled(err); got != tc.unbilled {
			t.Fatalf("%d: unbilled %v, want %v", tc.status, got, tc.unbilled)
		}
	}
}
