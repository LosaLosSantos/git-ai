package chat

// The language-model backends the chat front end can use. Each registers
// itself in init; the provider is chosen by Config.Provider.
import (
	_ "github.com/feder-cr/open_scion/tools/graft-agents/llm/anthropic"
	_ "github.com/feder-cr/open_scion/tools/graft-agents/llm/command"
	_ "github.com/feder-cr/open_scion/tools/graft-agents/llm/openai"
)
