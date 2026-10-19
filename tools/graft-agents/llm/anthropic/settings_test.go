package anthropic

import (
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/llm"
)

// TestNewRejectsSettingsItCannotTake checks that no setting is dropped silently. (Effort and
// max_tokens reach the request body: TestCompleteRequestShape and
// TestRequestMaxTokensOverridesDefault assert them.)
func TestNewRejectsSettingsItCannotTake(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  llm.Config
		want string
	}{
		"command": {llm.Config{Type: "anthropic", Command: []string{"claude"}}, "command is for the command provider"},
		"routing": {llm.Config{Type: "anthropic", Routing: &llm.Routing{Only: []string{"openai"}}}, "routing is OpenRouter's"},
		"options": {llm.Config{Type: "anthropic", Options: map[string]string{"z": "1", "a": "2"}}, "unsupported options [a z]"},
	} {
		_, err := New(tc.cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
