// Package chat answers questions about a Graft component for its team. It
// serves Slack slash commands (/graft <question>) and Events API app_mention
// events, and answers from the component's sources: recent changesets,
// verification state, graft_ metrics and log tails. The context gathered from
// the sources is redacted, bounded and handed to one llm.Provider call whose
// system prompt treats it as untrusted data. Without a provider, answers are a
// deterministic summary of the sources.
//
// The operator-facing description is in doc/graft/SUPERFAN-CHAT.md.
package chat
