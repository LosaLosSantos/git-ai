//go:build cgo

package resolve

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// refusingProvider fails every call with err.
type refusingProvider struct{ err error }

func (p refusingProvider) Name() string { return "refusing" }

func (p refusingProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, p.err
}

// statusErr is an llm.StatusError.
type statusErr struct {
	status int
	before bool
}

func (e statusErr) Error() string          { return fmt.Sprintf("HTTP %d", e.status) }
func (e statusErr) HTTPStatus() int        { return e.status }
func (e statusErr) BeforeGeneration() bool { return e.before }

// TestBudgetProviderBooksFailuresByClass: a call the provider refused before generating anything
// with a status it does not bill (400, 401, 402, 403) costs nothing, and its worst case is
// released; every other failure keeps the worst case, as it may have been billed. Every 402 is
// counted, so that a run can stop at the first.
func TestBudgetProviderBooksFailuresByClass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		released bool
		payment  bool
	}{
		{"400 before generation", statusErr{400, true}, true, false},
		{"401 before generation", statusErr{401, true}, true, false},
		{"402 before generation", statusErr{402, true}, true, true},
		{"403 before generation", statusErr{403, true}, true, false},
		{"wrapped 402", fmt.Errorf("llm: %w", statusErr{402, true}), true, true},
		{"402 in the middle of a stream", statusErr{402, false}, false, true},
		{"400 in the middle of a stream", statusErr{400, false}, false, false},
		{"429", statusErr{429, true}, false, false},
		{"500", statusErr{500, true}, false, false},
		{"503", statusErr{503, true}, false, false},
		{"timeout", context.DeadlineExceeded, false, false},
		{"transport error", errors.New("connection reset by peer"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := NewBudget(0, wp4Prices, "")
			if err != nil {
				t.Fatal(err)
			}
			bp := budgetProvider{p: refusingProvider{err: tc.err}, b: b}
			_, callErr := bp.Complete(context.Background(), llm.Request{Prompt: "x", MaxTokens: 1000})
			if !errors.Is(callErr, tc.err) {
				t.Fatalf("err = %v, want the provider's %v", callErr, tc.err)
			}
			// The worst case: (1 + 256) input tokens at $1 per million and 1000 output tokens at $10.
			worst := wp4USD(t, "0.010257")
			want := worst
			if tc.released {
				want = 0
			}
			if got := wp4Spent(t, b); got != want {
				t.Fatalf("spent %v, want %v", got, want)
			}
			if got := b.PaymentRefusals() > 0; got != tc.payment {
				t.Fatalf("payment refusals counted: %v, want %v", got, tc.payment)
			}
		})
	}
}
