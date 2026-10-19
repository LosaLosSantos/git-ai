package anthropic

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

const errorBody = `{"type":"error","error":{"type":"billing_error","message":"no credit"}}`

// TestErrorStatusIsReported: an error status of the API reaches the caller as an llm.StatusError,
// unbilled when the API refused the request before generating anything, as a refusal of a
// streamed request is too; an error in the middle of a stream is not.
func TestErrorStatusIsReported(t *testing.T) {
	for _, tc := range []struct {
		name      string
		maxTokens int
		respond   func(int, map[string]any) (int, string)
		status    int
		unbilled  bool
	}{
		{"refused request", 1000, func(int, map[string]any) (int, string) { return http.StatusPaymentRequired, errorBody }, 402, true},
		{"refused streamed request", 20000, func(int, map[string]any) (int, string) { return http.StatusForbidden, errorBody }, 403, true},
		{"error in the middle of a stream", 20000, func(int, map[string]any) (int, string) {
			stream := sse([]string{"partial"}, "end_turn")
			cut := strings.Index(stream, "event: content_block_stop")
			return http.StatusOK, stream[:cut] + "event: error\ndata: " + errorBody + "\n\n"
		}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t, tc.respond)
			p := newProvider(t, f)
			_, err := p.Complete(context.Background(), llm.Request{Prompt: "x", MaxTokens: tc.maxTokens})
			if err == nil {
				t.Fatal("expected an error")
			}
			if status, ok := llm.HTTPStatus(err); tc.status != 0 && (!ok || status != tc.status) {
				t.Fatalf("status %d (%v), want %d: %v", status, ok, tc.status, err)
			}
			if got := llm.Unbilled(err); got != tc.unbilled {
				t.Fatalf("unbilled %v, want %v: %v", got, tc.unbilled, err)
			}
		})
	}
}
