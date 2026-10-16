package superfan

// The llm summarizer builds its model backend from its options, so the
// provider types must be registered in every program that loads a superfan
// configuration with that summarizer. These blank imports do it for
// cmd/graft-superfan and for any program that imports this package.
import (
	_ "github.com/feder-cr/git-ai/tools/graft-agents/llm/anthropic"
	_ "github.com/feder-cr/git-ai/tools/graft-agents/llm/command"
	_ "github.com/feder-cr/git-ai/tools/graft-agents/llm/openai"
)
