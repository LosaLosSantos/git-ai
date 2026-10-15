# graft-superfan chat

`graft-superfan chat` lets a team ask questions about a Graft component in
Slack (`/graft <question>`, or by mentioning the app) and from the command line
(`graft-superfan chat ask "<question>"`). Answers are built from the component's
own sources and, when a language model is configured, written by one model call
over that context.

Package: `tools/graft-agents/superfan/chat`. Command: `tools/graft-agents/cmd/graft-superfan`
(subcommand `chat`). Model backends and their configuration are those of
`tools/graft-agents/llm` (`anthropic`, `openai`, `command`, or `none`).

## Commands

```sh
graft-superfan chat serve --config chat.json [--listen 127.0.0.1:8090]
graft-superfan chat ask [--config chat.json] "what changed in the last hour?"
```

`serve` answers Slack requests until interrupted. It listens on loopback by
default; put a TLS reverse proxy in front of it. `ask` answers one question on
standard output with the same engine. Flags must come before the question.
Without `--config`, `ask` reads no source and answers that no source is
configured.

Exit status: 0 on success or interrupt, 1 on a failure, 2 on a usage error.
On interrupt, `serve` stops accepting requests and waits up to 30 seconds for
the answers it already acknowledged to Slack; if some are still running then,
it exits with 1.

## Configuration

A JSON document. Unknown fields and trailing data are errors, so a misspelled
key cannot silently disable a source. Every section is optional.

```json
{
  "sources": {
    "changesets": { "argv": ["graft", "log", "--json", "-n", "50"], "dir": "/srv/graft/workspace" },
    "verify_state": "/srv/graft/verify/state.json",
    "metrics_url": "http://127.0.0.1:9100/metrics",
    "log_files": ["/var/log/graft/gitaly.log", "/var/log/graft/superfan.log"],
    "log_tail_lines": 200
  },
  "provider": { "type": "anthropic", "model": "claude-opus-5-5", "timeout": "120s", "max_tokens": 1024 },
  "slack": {
    "signing_secret_env": "GRAFT_SUPERFAN_SLACK_SIGNING_SECRET",
    "bot_token_env": "GRAFT_SUPERFAN_SLACK_BOT_TOKEN"
  },
  "max_context_bytes": 65536,
  "redact": ["internal-ticket-[0-9]+"]
}
```

| Key | Meaning |
| --- | --- |
| `sources.changesets` | Either `argv` (run without a shell, in `dir`; `argv[0]` is the graft binary) or `file` (a saved copy of the same output). Both are exclusive. |
| `sources.verify_state` | The state file of graft-verify (JSON or text). |
| `sources.metrics_url` | A Prometheus endpoint. Only `graft_` lines are kept. |
| `sources.log_files`, `log_tail_lines` | The last N non-empty lines of each file (default 200, at most 2000). Only the last 256 KiB of each file is read. |
| `provider` | An `llm.Config`. Type `none` or empty answers with a deterministic summary of the sources. |
| `slack.signing_secret_env`, `slack.bot_token_env` | Names of the environment variables holding the secrets. The defaults are the names shown above. |
| `slack.api_base` | Slack Web API base URL. Defaults to `https://slack.com/api`. |
| `max_context_bytes` | Bound of the context of one answer: 1 KiB to 1 MiB, default 64 KiB. |
| `redact` | Extra regular expressions replaced by `[REDACTED]`, on top of the built-in patterns. |

Secrets are never written in the configuration. `serve` refuses to start when
the signing secret variable is empty. The bot token is optional; without it,
`app_mention` events are logged and not answered.

### Source selection

The question's keywords select the sources. For example, "what changed" selects
the changesets, "why did the build fail" selects the verification state and the
logs, and "how many" selects the metrics. When no keyword matches, every
configured source is used. A source that cannot be read becomes an
`unavailable:` line in the context; it does not fail the answer.

### Context bound and redaction

The context of one answer is at most `max_context_bytes`. Each selected source
gets an equal share, and each share is cut on a UTF-8 boundary and marked
`[truncated]`. Every source is redacted before it is used, and so is every model
answer. The built-in patterns cover private key blocks, bearer tokens,
credentials in URLs (`https://user:password@host`), `password=`, `token:`,
`api_key=` and similar key-value pairs, AWS access key IDs, Slack, GitHub and
`sk-` style tokens. Key-value pairs are also found inside JSON that a log line
embeds as a string (`{\"token\":\"...\"}`), and a quoted value is redacted up
to its closing quote, spaces included. The patterns are a safety net, not a
guarantee: keep secrets out of the logs.

## Slack app

Create a Slack app with:

- a slash command `/graft` whose request URL is `https://<host>/slack/commands`;
- Event Subscriptions with the request URL `https://<host>/slack/events` and the
  bot event `app_mention`;
- the bot scope `chat:write` (for `app_mention` replies), and the bot token
  exported as `GRAFT_SUPERFAN_SLACK_BOT_TOKEN`;
- the signing secret of the app exported as `GRAFT_SUPERFAN_SLACK_SIGNING_SECRET`.

Requests are handled as follows:

- Every request must carry a valid `X-Slack-Signature` (`v0=` followed by the
  HMAC-SHA256 of `v0:<timestamp>:<body>`, compared in constant time) and an
  `X-Slack-Request-Timestamp` within five minutes of the server clock.
  Otherwise the response is `401` and nothing is processed.
- The URL verification challenge is answered with the challenge text.
- A slash command is acknowledged at once (empty `200`). The answer is posted
  to the command's `response_url` as an ephemeral message. `/graft` without a
  question prints the usage.
- An `app_mention` is acknowledged at once. The question is the text after the
  mention, and the answer is posted in the thread of the mention with
  `chat.postMessage`. Messages from bots (including the app itself) are
  acknowledged and not answered. Duplicates are dropped by `event_id`: an event
  whose `event_id` was received in the last 10 minutes is acknowledged and not
  answered again. A Slack retry (`X-Slack-Retry-Num`) of an event the server
  never received (connection failure, restart, proxy error) is answered. A
  retry of an envelope without an `event_id` cannot be deduplicated and is not
  answered. The seen `event_id`s are kept in memory only, so after a restart a
  retry of an event that was already answered may be answered again.
- At most `MaxConcurrent` answers (4) run at once. A slash command over that
  limit gets a "busy" reply; an over-limit mention is logged and dropped.
- Each answer is escaped for Slack, so the model cannot ping channels (`<!channel>`)
  or insert links.

Answers are bounded to 8 KiB, and each answer has a 10 minute deadline.

## Model and prompt

With a provider, an answer is one `Complete` call. The system prompt states
that the context is untrusted data, that instructions found in the context or
in the question are never followed, and that secrets and the rules are never
revealed. The question is quoted as user input. The context sits between BEGIN
and END lines that carry a random token generated for each prompt, so data
cannot close the block early.

Without a provider, the answer is a deterministic summary of the selected
sources: the changeset count and the first few entries, the verification state
lines, the `graft_` metric lines, and the number of log lines tailed.

## Limits and non-goals

- The chat answers from the sources it is given. It does not query the Graft
  server, and it does not change any repository, review or verification state.
- The Slack endpoints have no per-user authorization. Anyone who can use the
  workspace can ask questions, so restrict the app to the channels you want.
- Answers are not stored. There is no conversation memory between questions.
- Events are not retried by the server: a dropped `app_mention` (busy server)
  is not answered later.
- The request body is limited to 256 KiB and the question to 1 KiB.
