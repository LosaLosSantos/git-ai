package resolve

import (
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// The shipped programs must be able to build the llm strategy from a
// configuration: the provider types are registered by the blank imports of
// llm_providers.go.
func TestLLMProviderTypesRegistered(t *testing.T) {
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
