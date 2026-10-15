package resolve

// The llm strategy builds its model backend from the [strategy.llm] table, so
// the provider types must be registered in every program that loads a
// configuration with that strategy. These blank imports do it for the
// reference programs (cmd/graft-resolve) and for any program that imports this
// package. The SDK-based Anthropic backend stays inside this module.
import (
	_ "github.com/feder-cr/open_scion/tools/graft-agents/llm/anthropic"
	_ "github.com/feder-cr/open_scion/tools/graft-agents/llm/command"
	_ "github.com/feder-cr/open_scion/tools/graft-agents/llm/openai"
)
