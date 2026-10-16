package superfan

import (
	"context"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// The shipped programs must be able to build the llm summarizer from a
// configuration: the provider types are registered by the blank imports of
// summarize_providers.go.
func TestSummarizerProviderTypesRegistered(t *testing.T) {
	have := map[string]bool{}
	for _, typ := range llm.Types() {
		have[typ] = true
	}
	for _, want := range []string{"anthropic", "command", "openai"} {
		if !have[want] {
			t.Errorf("provider type %q is not registered (have %v)", want, llm.Types())
		}
	}
}

// Without max_tokens the request carries no limit, so the provider default
// applies (a fixed 1024 truncates reasoning models).
func TestSummarizeDigestDefaultMaxTokensIsProviderDefault(t *testing.T) {
	fake := wp19SetSumAnswer(func(_ context.Context, _ llm.Request) (*llm.Response, error) {
		return summaryAnswer("quiet window", "low"), nil
	})
	if _, err := SummarizeDigest(context.Background(), fake, []byte(`{"Component":"x"}`), DigestSummaryOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := fake.last().MaxTokens; got != 0 {
		t.Fatalf("max tokens = %d, want 0 so that the provider default applies", got)
	}
}
