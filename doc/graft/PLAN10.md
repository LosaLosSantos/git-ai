# Graft c1: AI conflict resolution with checks, measured (approved plan)

Status: approved by the PM with the amendments of section 12; WP1 to WP4 under
way. Owner: c1 (Opus). Worktree `owt/c1`, branch `c1-conflicts` from `main` at
v0.2.0 (`7baee7e`).

c1 owns `tools/graft-agents/resolve/**`, `tools/graft-agents/cmd/graft-resolve`,
`doc/graft/RESOLVE.md` and this plan; for the conflict package only, the land
conflict path in `internal/gitaly/service/operations` (`land*.go`, the error
details) and new fields and messages in `proto/` (section 6.1, a public API).
The land decision itself does not change; anything that would touch it is
coordinated with the PM first.

## 1. Goals (Federico's final decisions, binding)

* G1 **One model plus automatic checks.** What git cannot merge goes straight to
  the `llm` strategy: one model through OpenRouter (the OpenAI-compatible
  provider). No second model, no judge. The default chain for AI resolution is the
  llm strategy alone; `trivial`, `union`, `rerere` and `command` stay available as
  opt-in configuration. No Mergiraf.
* G2 **No self-declared confidence.** The confidence field leaves the prompt, the
  answer schema, the parsing and the docs, and the low-confidence rejection goes
  with it. Acceptance comes from the checks only.
* G3 **Checks before the land, on an AI resolution: syntax valid, plus no conflict
  markers.** The result must parse with tree-sitter for the file's language without
  an error node; a file in a language without a grammar gets the marker rule only,
  as RESOLVE.md states. No formatter, no build gate.
* G4 **When the server cannot resolve**, the land fails with a complete conflict
  package for the agent (per file the base/ours/theirs and the conflicting hunks,
  both commit messages, the merge base and the tip, what was tried and why it
  failed), plus the SDK and CLI path to resolve locally and land again.
* G5 **Lockfiles always go to the agent** (`package-lock.json`, `yarn.lock`,
  `pnpm-lock.yaml`, `poetry.lock`, `Cargo.lock`, `go.sum`): never sent to a model,
  never merged by the server. No lockfile strategy.
* G6 **Never sent to the model by default**: large, binary, generated and minified
  files (detection in 3.4). Secrets, keys and configuration may be sent; RESOLVE.md
  states plainly that file contents, secrets included, go to the provider.
* G7 **Merged-tree build/test before the land stays optional, off by default.** The
  safety net of an AI resolution that lands without review is the trail
  (`Graft-Resolved-*` trailers, the log) and graft-verify's bisect and revert.
* G8 **Measure it once**: the llm strategy with `openai/gpt-5-mini` through
  OpenRouter, on real conflicts in TypeScript/JavaScript, Python, Go and
  Rust/Java, about 200 conflicts, under a hard cap of $10 that the run enforces.
  Report correct / wrong / rejected-by-checks / not sent / model error, so the real
  error rate of this setup is known. Federico gives the key when the benchmark is
  ready.
* G9 **A monthly spending cap in production.** graft-resolve tracks the spend of AI
  resolution from the provider's reported usage times a configured price, persisted
  across restarts, with a metric; when the month's cap is reached, conflicts go
  straight to the agent package with the reason "AI budget exhausted".

## 2. What exists today (from the code)

* `UserLand` with `resolve_conflicts` computes the conflicting files of the merge of
  the tip and the landed commit, with their base/ours/theirs blobs, and sends
  protocol v1 to the resolver, both commit messages included (`Messages`). A valid
  answer becomes a merge commit with `Graft-Resolved-By` and `Graft-Resolved-Path`
  trailers (`land_resolve.go`).
* On any failure the client gets the plain `MergeConflictError`
  (`conflicting_files`, `conflicting_commit_ids`); the reason (a denied path, the
  strategy that failed and why) is only logged by the server. The agent cannot learn
  what was tried, nor get the sides through the error.
* The llm strategy prompts with the path and the three sides only
  (`llmWholePrompt`, `llmHunkPrompt`); the commit messages are not used. Its answer
  has `content`, `confidence`, `explanation`; low confidence is rejected, medium
  accepted. Large files go hunk by hunk, so one file can cost several calls.
* graft-resolve rejects any result with conflict markers (git's rule) and runs
  configured per-glob checks; nothing parses the result.
* The OpenAI-compatible provider takes `base_url`, `model`, `api_key_env`,
  `max_tokens`, `effort` and reads the usage of each answer.
* The server sets no gRPC message size, so grpc-go's default of 4 MiB applies to a
  request it receives.
* graft-verify bisects a failing tip and reverts the culprit (opt-in).
* No measurement of the llm strategy exists.

## 3. Design

### 3.1 The llm strategy without confidence, with the messages (G1, G2)

* The answer schema becomes `{"content": "...", "explanation": "..."}`. The prompts
  no longer ask for a confidence, and no field of the answer decides acceptance; an
  answer with extra fields is still read (models add fields). The options
  `require_confidence` and the like are removed, and a configuration that still sets
  them fails at startup with a message that says they are gone.
* Both commit messages go into the prompt by default (PM, Q3) as the intent of each
  side, bounded to 4 KiB each and marked as untrusted data like the file contents.
  They cost no extra call.
* The AI setup documented in RESOLVE.md is `[[strategy]] type = "llm"` alone; the
  deterministic strategies are documented as opt-in.

### 3.2 The syntax check (G3)

* Built into graft-resolve and always on for llm results: the content is parsed
  with the tree-sitter grammar chosen by the path's extension; any ERROR or MISSING
  node rejects the result, with the line and column of the first one. Grammars:
  TypeScript and TSX (`.ts`, `.mts`, `.cts`, `.tsx`), JavaScript (`.js`, `.mjs`,
  `.cjs`, `.jsx`), Python (`.py`, `.pyi`), Go (`.go`), Rust (`.rs`), Java (`.java`).
  Every other extension gets the marker rule only.
* Bindings: the official MIT Go bindings `github.com/tree-sitter/go-tree-sitter`
  and the grammar modules of those languages, with cgo, in the `tools/graft-agents`
  module only (a separate module and binary). The main gitai module gets no new
  dependency. RESOLVE.md states the build requirement (a C compiler, `CGO_ENABLED=1`
  for graft-resolve) and what a language without a grammar gets.
* The marker rule stays as it is, for every strategy.

### 3.3 The agent conflict package and the re-land (G4)

The server already computes everything. The error carries a small descriptor and
the agent fetches the contents by object ID, so no file content travels in the gRPC
error, whose size is limited.

* The error of a land that conflicts keeps `UserLandError.merge_conflict` unchanged
  (upstream clients rely on it) and carries a **second detail**, a `LandConflict`
  (6.1), when the request set `resolve_conflicts`: gRPC status details are a list, so
  existing clients see nothing new.
* `LandConflict` holds the tip, the landed commit, the merge base, both commit
  messages (cut to 4 KiB, with a flag when cut), and per file the path, the kind,
  each side's blob OID and mode, the conflicting hunks as line ranges of each side,
  and the outcome of the server's attempt with its reason: not sent (lockfile,
  binary, large, generated, minified, denied path, invalid UTF-8 path), budget
  exhausted, model error, rejected by checks (syntax error at line:column,
  conflict markers), resolved but not landed because another file was not, or not
  attempted because the whole request was refused (too many files, actor not
  allowed, resolution disabled), with the request-level reason. Reasons are cut to
  512 bytes. The package lists every conflicting file, within its own bounds (see
  "Review of WP5/WP6" in section 12).
* The agent reads the sides with `GetBlobs`, which its read access allows.
* **Re-land**: `UserLandRequest.resolution` (`LandResolution`) carries the tip and
  the commit the resolution was made against and, per file, `write` (mode, content)
  or `delete`. The server accepts it only when `commit_id` equals the resolution's
  commit and the merge of the current tip and that commit conflicts on exactly those
  paths. It validates it with the rules of a resolver answer (markers, modes, sizes;
  no syntax check, the agent owns its answer) and lands a merge commit with
  `Graft-Resolved-By: agent/<actor>`; the trailer path is the one of
  `land_resolve.go`. If the tip moved, the land fails with a fresh package. The
  resolution travels in the request, so it is bounded by the 4 MiB gRPC limit in
  total (RESOLVE.md says so); larger resolutions are a stated limit of this plan.
* **SDK**: the land error exposes `Conflict()` (the package) and
  `ConflictSides(ctx)` (the contents); `LandOptions.Resolution` re-lands. **CLI**:
  `graft land` prints the package; `--conflicts-dir DIR` writes each file with
  git-style markers and the three sides beside it (`.base`, `.ours`, `.theirs`);
  `graft land --resolved DIR` lands the edited files as the resolution.

### 3.4 What is never sent (G5, G6)

The server decides per file before the request, so the resolver never sees these
files, and the package names the reason:

* binary: a NUL byte in any side;
* large: any side over `max_file_bytes` (default 1 MiB, as today);
* lockfile: the base name is one of `package-lock.json`, `yarn.lock`,
  `pnpm-lock.yaml`, `poetry.lock`, `Cargo.lock`, `go.sum` (fixed list, extendable
  with `lockfiles` in the configuration);
* generated: `linguist-generated` is set for the path by the `.gitattributes` of the
  tip's tree (git check-attr against the tip), or a side's first 10 lines hold one of
  `Code generated .* DO NOT EDIT\.` (Go's convention), `@generated`, `DO NOT EDIT`,
  `auto-generated` (extendable with `generated_markers`);
* minified: the path matches `*.min.js`, `*.min.mjs`, `*.min.css` or `*.bundle.js`,
  or a side of more than 2 KiB has an average line length above 300 bytes or a line
  longer than 5,000 bytes.

When a request has such a file, the other files still go to the model; the land
fails if any file is left, and the package shows which files were resolved and which
need the agent. Nothing partial lands.

### 3.5 The monthly spending cap (G9)

* `resolve.toml`, llm strategy: `[strategy.llm.budget]` with `monthly_usd` (the cap;
  0 or absent means none), `input_usd_per_million` and `output_usd_per_million` (the
  prices of the configured model, which the operator keeps current), and `state`
  (the file that holds the spend).
* Before a call the strategy reserves the worst case of that call (the request's
  estimated input tokens times the input price plus `max_tokens` times the output
  price); if the month's spend plus the reservation exceeds the cap, the strategy
  does not call and abstains with "AI budget exhausted". After the call it books the
  real cost from the reported usage (input and output tokens, reasoning tokens
  included in the output) and releases the reservation. A provider that reports no
  usage is booked at the reservation.
* The state file is JSON (`{"month": "2026-10", "spent_usd": ...}`), updated under
  an flock and written atomically with a temporary file and a rename, fsynced, so the
  spend survives restarts and concurrent graft-resolve processes share it. A new
  calendar month (UTC) starts at zero.
* Metrics: `graft_resolve_ai_spend_usd` (gauge, the month's spend),
  `graft_resolve_ai_budget_usd` (gauge, the cap) and
  `graft_resolve_ai_budget_exhausted_total` (counter). graft-resolve exposes them in
  HTTP mode on `/metrics`; in command mode the spend is in the state file and in the
  `explanation` of each answer.
* The server sees "AI budget exhausted" as the resolver's failure reason and puts it
  into the package.

### 3.6 Optional merged-tree verification (G7)

`[graft.land.resolver.verify]`: `enabled = false` (default), `command` (argv run in a
temporary checkout of the merged tree, minimal environment), `timeout` (default 5
min, hard). A failure or timeout rejects the resolution, and the package says so.
RESOLVE.md gets "finding and reverting AI resolutions": the trailers (`git log
--grep='^Graft-Resolved-By:'` in a clone, `graft show --json`; `graft log` lists
changesets and has no grep), the info log, graft-verify's bisect and revert.

### 3.7 The benchmark (G8)

**Corpus.** Merge-Bench's data location and license are not published on its
abstract page, so the corpus is mined: a committed corpus file lists well-known
repositories with permissive licenses (MIT, Apache-2.0, BSD) per family, with a
pinned commit and the license; `graft-resolve bench mine` clones them (partial
clones), replays each two-parent merge with `git merge-tree --write-tree` on its
parents, and keeps every conflicting text file whose human resolution (the merge
commit's version) differs from both sides. Only a manifest is committed (repository,
commit IDs, path, license); no third-party code. Candidates, to be kept if they yield
enough conflicts (many projects squash their pull requests, which leaves no
conflicting merges; the miner reports the yield per repository):

* TypeScript/JavaScript: microsoft/TypeScript (Apache-2.0), microsoft/vscode (MIT),
  facebook/react (MIT), nodejs/node (MIT, its own code);
* Python: django/django (BSD-3-Clause), pandas-dev/pandas (BSD-3-Clause),
  python/cpython (PSF-2.0, permissive) as a fallback;
* Go: kubernetes/kubernetes (Apache-2.0), prometheus/prometheus (Apache-2.0),
  golang/go (BSD-3-Clause);
* Rust/Java: rust-lang/rust-analyzer (MIT/Apache-2.0), tokio-rs/tokio (MIT),
  apache/kafka (Apache-2.0), spring-projects/spring-boot (Apache-2.0).

**Run.** `graft-resolve bench run` builds the protocol v1 request of each sampled
case as the server would (never-send rules included), runs the configured llm
strategy with its checks, and judges each accepted result against the human file:

* correct: equal to the human file after normalizing line endings and trailing
  whitespace (exact equality reported beside it);
* wrong: accepted by the checks but not equal;
* rejected by checks: syntax error or markers;
* not sent: lockfile, binary, large, generated, minified;
* model error: the call failed or the answer could not be read;
* not run (cap).

The report gives these per family and in total, and the error rate = wrong /
accepted: a wrong answer lands without review.

**Sample and the $10 cap.** A fixed, seeded sample of 200 cases stratified by family
(`--sample 200 --seed S`). The run enforces `--max-usd 10` with the prices of the
model (`--input-usd-per-million 0.25 --output-usd-per-million 2`, OpenRouter's list
price for `openai/gpt-5-mini`; the run takes the prices as flags so that a changed
price is entered, not assumed): before each call it reserves the worst case (the
estimated input plus `max_tokens` of output, reasoning included) and does not call
if the spend plus the reservation would exceed $10; after the call it books the
reported usage. `max_tokens` per call is 8,000 and `effort` "low" to bound reasoning.
The key is read from `OPENROUTER_API_KEY`, never stored.

**Estimated cost.** 200 cases, about 1.5 calls per case (hunk mode for large files),
3,000 input tokens and about 4,500 output tokens per call (1,500 of answer and up to
3,000 of reasoning): 300 × (3,000 × $0.25 + 4,500 × $2) / 1,000,000 ≈ 300 ×
$0.00975 ≈ $2.9. The worst case of every call at `max_tokens` is 300 × (0.00075 +
0.016) ≈ $5.0, so the $10 cap leaves room and is the hard bound whatever the files
hold.

## 4. Invariants

* I1 The land decision, the CAS and the trailers are unchanged; nothing partial
  lands.
* I2 A lockfile, a binary, large, generated or minified file never reaches the
  resolver.
* I3 An AI result lands only when it parses (where a grammar exists) and has no
  markers.
* I4 Every failure of a land that asked for resolution gives the agent the package;
  the agent's re-land is validated like a resolver answer.
* I5 AI spend never exceeds the monthly cap, across restarts and processes; the
  benchmark never exceeds its $10.
* I6 The benchmark is reproducible from the corpus file, the manifest and the seed.

## 5. Failure and race cases

| case | behaviour |
|---|---|
| tip moves between the package and the re-land | the re-land fails with a fresh package |
| the agent's resolution names other paths than the conflict | `InvalidArgument`, package again |
| the model returns a file that does not parse | rejected by checks, package with line:column |
| the model times out or errs | model error, package |
| a request mixes sendable files and a lockfile | the sendable ones are resolved, the land fails with the package |
| the monthly cap is reached | no call, "AI budget exhausted", package |
| two graft-resolve processes spend at once | the state file's flock serializes the reservations |
| the state file is corrupt | the strategy refuses to call (budget unknown) and abstains with the reason |
| the benchmark reaches $10 | it stops calling; the rest is "not run (cap)" |

## 6. Config and API changes

### 6.1 Proto (exact, for the PM's review)

`proto/operations.proto`, in `UserLandRequest`:

```proto
  // resolution lands commit_id with the caller's resolution of the conflicts that a
  // LandConflict described, instead of asking the server's resolver. The server
  // accepts it only when commit_id equals resolution.commit and the merge of the
  // current tip and commit_id conflicts on exactly the paths of resolution.files;
  // otherwise the land fails with a fresh LandConflict. It is validated like a
  // resolver answer (no conflict markers, modes 100644, 100755 or 120000, sizes) and
  // lands as a merge commit with a Graft-Resolved-By: agent/<actor> trailer.
  LandResolution resolution = 11;
```

New messages in `proto/operations.proto`:

```proto
// LandConflict describes the conflicts of a land that the server did not resolve, so
// that the caller can resolve them and land again with UserLandRequest.resolution. It
// is sent as an additional detail of the error, next to UserLandError, when the
// request set resolve_conflicts.
message LandConflict {
  // tip is the branch tip that the change was merged with.
  string tip = 1;
  // commit is the landed commit.
  string commit = 2;
  // merge_base is the merge base of tip and commit, empty for unrelated histories.
  string merge_base = 3;
  // tip_message and commit_message are the commit messages, cut to 4 KiB each.
  bytes tip_message = 4;
  bytes commit_message = 5;
  // messages_truncated is set when a message was cut.
  bool messages_truncated = 6;
  // files are all the conflicting files (see "Review of WP5/WP6" in section 12).
  repeated LandConflictFile files = 7;
  // reason is set when the server did not try to resolve the request at all, for
  // example because it has too many files or the actor may not resolve; it is cut to
  // 512 bytes.
  string reason = 8;
}

// LandConflictFile is one conflicting file of a LandConflict.
message LandConflictFile {
  // Kind is the kind of the conflict.
  enum Kind {
    KIND_UNSPECIFIED = 0;
    KIND_CONTENT = 1;
    KIND_MODIFY_DELETE = 2;
    KIND_ADD_ADD = 3;
    KIND_MODE = 4;
  }
  // Outcome is what the server did with the file.
  enum Outcome {
    OUTCOME_UNSPECIFIED = 0;
    // OUTCOME_NOT_SENT: the file was not sent to the resolver (reason: lockfile,
    // binary, large, generated, minified, denied path, invalid UTF-8 path).
    OUTCOME_NOT_SENT = 1;
    // OUTCOME_REJECTED: the resolver's result failed a check (reason: the check).
    OUTCOME_REJECTED = 2;
    // OUTCOME_FAILED: the resolver failed on the file (reason: the error, such as a
    // model error or "AI budget exhausted").
    OUTCOME_FAILED = 3;
    // OUTCOME_RESOLVED: the file was resolved, but the land failed because another
    // file was not.
    OUTCOME_RESOLVED = 4;
    // OUTCOME_NOT_ATTEMPTED: the request was not resolved at all (see
    // LandConflict.reason).
    OUTCOME_NOT_ATTEMPTED = 5;
  }
  bytes path = 1;
  Kind kind = 2;
  // base, ours and theirs are the sides; an absent side is unset. ours is the tip's
  // side, theirs the landed commit's.
  LandConflictSide base = 3;
  LandConflictSide ours = 4;
  LandConflictSide theirs = 5;
  // hunks are the conflicting regions, in order.
  repeated LandConflictHunk hunks = 6;
  Outcome outcome = 7;
  // reason explains the outcome, cut to 512 bytes.
  string reason = 8;
}

// LandConflictSide is one side of a conflicting file; its content is read by oid.
message LandConflictSide {
  string oid = 1;
  string mode = 2;
}

// LandConflictHunk is a conflicting region as line ranges of each side, 1-based and
// half-open ([start, end)); an empty range (start == end) is an insertion point.
message LandConflictHunk {
  LineRange base = 1;
  LineRange ours = 2;
  LineRange theirs = 3;
}

// LineRange is a half-open range of 1-based lines.
message LineRange {
  uint32 start = 1;
  uint32 end = 2;
}

// LandResolution is the caller's resolution of the conflicts of a LandConflict.
message LandResolution {
  // tip is the tip that the resolution was made against, from LandConflict.tip.
  string tip = 1;
  // commit is the landed commit, LandConflict.commit; it must equal commit_id.
  string commit = 2;
  // files resolve every conflicting path exactly once.
  repeated LandResolvedFile files = 3;
}

// LandResolvedFile resolves one conflicting path.
message LandResolvedFile {
  // Action is what becomes of the path.
  enum Action {
    ACTION_UNSPECIFIED = 0;
    ACTION_WRITE = 1;
    ACTION_DELETE = 2;
  }
  bytes path = 1;
  Action action = 2;
  // mode is 100644, 100755 or 120000 for ACTION_WRITE.
  string mode = 3;
  bytes content = 4;
}
```

`UserLandResponse` is unchanged: a land with a resolution reports its paths in
`resolved_paths` and `agent/<actor>` in `resolver`, as a resolved land does today.

### 6.2 Configuration and flags

* Server `[graft.land.resolver]`: `lockfiles`, `generated_markers`, `minified`
  (globs and thresholds), `[graft.land.resolver.verify]`.
* `resolve.toml`: the llm strategy without confidence options;
  `[strategy.llm.budget]` (3.5). The syntax check has no option.
* `graft-resolve bench mine|run`: `--corpus`, `--manifest`, `--sample`, `--seed`,
  `--max-usd`, `--input-usd-per-million`, `--output-usd-per-million`,
  `--json-out`, `--md-out`.

## 7. Work packages

* WP1 (Opus): miner, manifest, corpus file per family with licenses; the bench
  runner with a fake provider (the $ cap tested); the report.
* WP2 (Opus): the llm strategy without confidence, the messages in the prompt.
* WP3 (Opus): the syntax check with go-tree-sitter and the grammars (cgo, in
  `tools/graft-agents` only); RESOLVE.md build requirement.
* WP4 (Opus): the monthly budget: reservation, booking, state file, metrics.
* WP5 (Opus): the server's never-send rules (3.4) with reasons.
* WP6 (Opus, proto of 6.1 after the PM's review): the package and the re-land; SDK
  and CLI.
* WP7 (Opus): optional merged-tree verification.
* WP8 (Opus + Haiku drafts): RESOLVE.md and the benchmark report.
* The measured run, once Federico gives the key (one-off, $10).

The order puts the benchmark first: WP1, WP2, WP3 and WP4 are what the measured run
needs; WP5 to WP8 follow, so that WP96 of PLAN9 can start once WP1 to WP4 are under
way.

## 8. Test plan

* Miner: a fixture repository with crafted merges (content, add-add, modify-delete,
  clean, binary, a lockfile, a generated file) gives exactly the expected cases; the
  manifest replays identically.
* Runner: a fake provider that reports usage, with prices, stops before the call that
  could pass the cap; the judgement on crafted pairs.
* llm strategy: answers without confidence accepted; removed options rejected at
  startup; the messages in the prompt, bounded.
* Syntax check: valid and broken files for each grammar; an unknown extension gets
  the marker rule only.
* Budget: reservation and booking with a fake provider; the cap reached; the state
  across a restart and two processes; a new month; a corrupt state file.
* Never-send: each rule with its reason; `.gitattributes` `linguist-generated` read
  from the tip.
* Package and re-land: every outcome and reason; re-land accepted; after the tip
  moved; wrong paths; markers; a resolution over the gRPC size.
* The existing resolve, land and e2e suites unchanged.

## 9. Acceptance

* The benchmark report per family: correct, wrong, rejected by checks, not sent,
  model error, not run (cap), error rate; within $10; corpus and manifest committed;
  the model and its prices named.
* The monthly cap enforced across restarts, with its metrics.
* The package and re-land end to end through the SDK and the CLI.
* All tests green; the land path and e2e unchanged.

## 10. Rejected alternatives (by Federico's decisions)

* Two-model agreement or a judge model; self-declared confidence.
* Deterministic or syntax-aware tiers (Mergiraf) before the model by default.
* A formatter or build gate before the land; a lockfile strategy; a secrets deny list
  by default.
* File contents in the gRPC error: limited size, and the agent can read the blobs.
* Replacing `merge_conflict` with the new package in `UserLandError`'s oneof: it
  would break the clients that read `merge_conflict`; an extra detail does not.

## 11. Risks and questions

* The corpus yield: projects that squash their pull requests have few conflicting
  merges; the miner reports the yield and the corpus keeps the repositories that
  give enough cases per family.
* The price of `openai/gpt-5-mini` is entered as a flag and in the budget
  configuration; if OpenRouter changes it, the operator updates it, and the cap is
  only as right as the configured price.
* A resolution larger than the 4 MiB gRPC limit cannot be re-landed through the
  request (a stated limit).

## 12. Amendments after approval (the PM's and Federico's decisions)

* **Model.** `openai/gpt-6.1-sol`, the current GPT on OpenRouter, replaces
  `openai/gpt-5-mini`. The model is a configuration value, so another current model
  (for example `openai/gpt-6-luna`) can be measured later without a code change.
  Its price is never taken from memory: `graft-resolve bench run` reads it at run
  time from OpenRouter's model API (`https://openrouter.ai/api/v1/models`, the
  `pricing` of the model's endpoints: prompt, completion and, when listed, internal
  reasoning, in USD per token), takes the highest of the endpoints as the worst
  case, writes it into the report, and refuses to run when the model or its price
  cannot be read. The production budget (3.5) keeps configured prices, which the
  operator sets from the same page. The hard $10 cap and the per-call worst-case
  reservation stay; with third-party figures of about $2 input and $10 output per
  million tokens the sample shrinks to what fits (about 150 to 200 cases), and the
  report states the final N.
* **Re-land with a resolution** (6.1, condition 1). The server accepts
  `LandResolution` only when `resolution.tip` equals the branch's current tip and
  `resolution.commit` equals `commit_id`; it may write only the paths that conflict at
  that tip, each exactly once, no other path; it runs the same validation as a
  resolver answer, the syntax check (3.2) included, besides markers, modes and sizes;
  and it records the merge commit with `Graft-Resolved-By: agent/<actor>`. Otherwise
  the land fails with a fresh package, never a partial write. (This replaces "no
  syntax check" in 3.3.) The syntax check therefore also runs in the server for an
  agent's resolution; since the main module takes no tree-sitter dependency, the
  server asks graft-resolve to check it (a `check` request of the resolver protocol)
  when a resolver is configured, and applies the marker rule only otherwise; WP6
  details it for the PM before implementing.
* **Hunk ranges** (6.1, condition 2). Each `LineRange` of a `LandConflictHunk` is
  relative to the side it names: `base` counts the lines of the base side's file,
  `ours` of the tip's, `theirs` of the landed commit's; 1-based, half-open
  ([start, end)), an empty range being an insertion point before line `start`. The
  proto comments state it.
* **The 4 MiB limit** (6.1, condition 3). RESOLVE.md states it, and the SDK refuses a
  resolution whose encoded request would exceed it, with a clear error, instead of a
  transport failure.
* **Order.** WP1 to WP4 first; WP96 of PLAN9 after them.
* **Per-file outcomes of a refusal** (PM, WP6). The resolver protocol gains an optional
  `files` list in the error response (HTTP 422 body, command-mode stdout): the outcome of
  every requested file, `resolved`, `rejected` or `failed`. The server trusts it only
  when it has exactly one entry per requested path with a known outcome, and otherwise
  marks every sent file failed with the overall reason. Backward compatible both ways.
* **Later improvement, not now** (PM). Since nothing lands partially, the files a
  resolver resolved in a failed request are wasted work for the agent. The package could
  offer the resolver's resolved content to the agent (for example as blobs written to the
  repository, referenced by OID), so that the agent re-lands with it where it agrees.
* **Review of WP5/WP6** (independent review of f707b5b and ad68d37, PM: fix all). What
  changed from the text above:
  * A refusal is never written. Only an accepted answer is, and a refusal whose outcomes
    mark every file resolved contradicts itself, so it is malformed (every file failed).
  * Every `Graft-*` trailer is written by one function, which refuses any value that is
    not one line of printable UTF-8. Such a path is never sent to a resolver, and the
    re-land refuses it (`FailedPrecondition`).
  * A side's blob OID is in the package only when the blob is in the tree of the merge
    base, the tip or the landed commit. A blob that merge-tree wrote for the conflict (the
    content merge of a rename) lives in the request's quarantine, which a failed land
    discards. Such a side gets `unavailable` with the reason; the CLI leaves it out
    without failing the other sides or files.
  * `actors` gates only the requests for an AI resolution. An agent's re-land is allowed
    for every actor that may land, and its `check` runs whatever the actor and covers all
    its files. `max_files` bounds the AI request only. The package lists every
    conflicting file, with bounds of its own: 1000 files (a refusal above), 1000 merge
    messages, 4 MiB encoded, 256 hunks per file, 64 MiB of sides read and 64 Mi cells of
    diff work for the hunks; `hunks_omitted` says why a file lacks hunks. The package is
    built only when the land returns it.
  * A merge that is unclean without a conflicting file (a directory rename split) gets a
    package that describes it by git's messages (`messages`), which every package carries.
  * Minors: a re-land with `STRATEGY_FF_ONLY` is `InvalidArgument`; a written mode must
    be one of the modes of the sides; a symbolic link named like `.gitmodules`,
    `.gitattributes`, `.gitignore` or `.mailmap` (in the ways HFS+ and NTFS fold names
    too) is refused; a symbolic link answer has syntax `none`; the
    `syntax_unchecked` metric counts once, on a successful land; merge3 writes CRLF
    markers where git does; the CLI layout of the sides is `DIR/sides/<side>/<path>`
    (`<path>.base` could collide with a directory named `<path>.base`), the CLI refuses
    paths it cannot write on its system and paths the file system folds together, and
    never overwrites a conflicts directory; the memory backend refuses a resolution for
    another commit.
* **Re-review of 54189d1** (PM: fix before R1).
  * The re-land of an agent is bounded: a merge with more than 1000 conflicting files is refused
    before anything is read, and its check goes in requests of at most 32 MiB of file data each,
    checked one after the other (resolver.MaxCheckRequestBytes = 64 MiB encoded, which the client
    also enforces), so that the server and graft-resolve hold one bounded request at a time.
  * The CLI never drops a side: without the base the file to edit is the two-way merge; without
    ours or theirs it is a conflict between markers (the side there is against a note), which the
    marker rule refuses until the agent resolves it.
  * A file with an unavailable side gets no hunks; the tree of every merge base counts as kept (a
    criss-cross base side that is a real blob keeps its OID); a gitlink side is unavailable.
  * Minors: the collision error names both paths; the colon is refused on Windows only; the NTFS
    names follow git's rules, the hashed 8.3 forms included; only the last paragraph of a message
    is its trailer block, and the server's trailers are always that paragraph (documented,
    tested).
* **Second re-review of 38d8bb1** (PM: fix before R1).
  * The check of an agent's resolution runs before the land enters its lane (landPrecheckResolution
    in UserLand), against resolution.tip: the only tip the land accepts the resolution at, so no
    re-check is needed. In the lane the land applies the verdicts when the tip and the commit are
    those the check ran on, and fails closed otherwise; if the branch moved, the resolution is
    refused with a fresh package before any verdict is used. No branch is held while the external
    process runs. The whole check has one deadline (the resolver's timeout); each request uses what
    remains, and expiry fails closed, the files checked before keeping their verdicts.
  * Check requests carry no commit message; their file data is bounded by resolver.CheckBatchBytes
    with a per-file allowance for paths, and max_file_bytes is refused at startup above
    resolver.MaxCheckFileBytes (about 12 MiB), so that one file always fits one request.
  * The listings of the trees and the attributes are read once per land, not per batch.
  * The CLI never falls back to one side: when it cannot merge a file, the file to edit is one diff3
    conflict holding every side (pointer lines for binary sides, the text of text sides, notes for
    deleted and unavailable ones). --resolved refuses a pointer line, and a path that is a file on
    none of its sides can only be deleted.
