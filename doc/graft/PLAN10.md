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
* **Review of e661706** (PM: fix before R1).
  * N1. A resolver configuration the server could not build is a startup error (config
    validation: empty command, URL without http(s) or host, unreadable or empty token file,
    max_file_bytes over the check bound, negative bounds). If resolver.New still fails at run time,
    the server keeps the error (resolverErr) and every land that needs the resolver fails closed
    with it: no path turns a configured resolver into "no resolver".
  * N2. The precheck runs in an object quarantine that UserLand discards. Before, merge-tree wrote
    the merged trees and the blobs with markers into the repository's own objects, unreachable,
    until gc, in every storage mode (off, shadow, authoritative: all keep objects in the local
    repository; they never reached the bucket, whose packs carry only the objects that new
    reference values reach). The lane still merges again in its own quarantine: reusing the precheck's tree
    would mean keeping its objects; correctness first.
  * N3. resolver.Client holds check_concurrency slots (default 2); a check takes one per request,
    its wait counts against the check's deadline, and its expiry fails closed; the wait is the
    histogram graft_resolver_check_slot_wait_seconds. The client's check is reachable only through
    a slot.
  * N4. Before any check the precheck reads the branch: a tip other than resolution.tip, or a change
    already landed, is not checked; the lane, which reads the tip again under the lane, refuses it
    (tip moved) or answers it (already landed).
  * N5. A failure of the precheck is the result's failure with its cause (a git error, the slot
    wait, a request), which the package reports, still failing closed; a precheck that was skipped,
    or made for another tip or commit, says so.
  * Partial 5: everything that stops the check between or during batches keeps the verdicts found
    before, the rest not attempted, and the land gets the package. Partial 7: the CLI's base note
    says "both sides added the file" only when both sides are there. Partial 8: tests for several
    files in one request, a file at the bound with a 4 KiB path fitting one request (one byte more
    refused by the configuration), and the collision test named for what it tests.
  * The classify cache is fixed to the land's tip and paths when made; another tip or path is an
    error. The pointer-line rule is in the server's validation (resolver.ConflictPointerPrefix), so
    clients of the SDK get it; the CLI keeps its earlier message.

* **Re-review of fcf5f8d** (PM: fix first).
  * The resolver client is built once per process (resolver.Shared in service.Dependencies, as the
    land lanes are): its check slots bound the process across listeners, and a configuration that
    cannot be built is one error. A server whose dependencies carry none builds its own.
  * A panic can no longer lose a check slot or the precheck's quarantine: both are released by
    defer (a test panics inside a batch and takes the only slot again).
  * The pointer-line rule exempts a file whose conflict has a side with such a line of its own, in
    Validate (sides from the request), on the server for agents' resolutions (the sides are read
    only when the rule fires) and in the CLI.
  * The precheck reads the branch as the lane does (ResolveRevision), so a branch that is a
    symbolic reference is checked at its target's tip.
  * A check request that runs out of the check's deadline says so; one that runs out of its own
    timeout names the timeout.

## 13. Prompt first (PM and Federico, approved)

Direction from Federico: the prompt first, then the models; adopt a structure that is already used
and measured instead of inventing one. The base is synthmerge's patch-application design (Andrea
Arcangeli, synthmerge): the only published resolver measured against real human resolutions, and
one that lands a change onto a tip, as a land does. synthmerge is GPL-3.0-or-later or AGPL: we take
the structure only, written in our own words, with our own worked example; RESOLVE.md credits it.

**R1, the structure** (`tools/graft-agents/resolve/llm_patch.go`, the only mode of the llm
strategy; the whole-file mode and the JSON answers are removed):
* One model call per conflicting region; the regions only one side changed are merged locally.
* PATCH: THEIRS' change to the region, a unified diff from BASE with 3 context lines (synthmerge's
  default). CODE: OURS' region with the same 3 lines before and after, taken from the file with
  every other conflicting region as OURS. The task: apply the PATCH to the CODE.
* System message: the worked example (its sections tagged EXAMPLE ..., so that it never reads as
  the answer), THEIRS' commit message, as synthmerge sends the change's message with its commit
  diff, the other conflicting regions of the file for reference, the directives last. User
  message: the PATCH, then the CODE. (This text first said "both commit messages", citing it as
  Federico's decision; the record is the PM's answer to Q3, "both commit messages in the prompt by
  default", not a decision of Federico's. On the PM's ruling R1 sends THEIRS' message only, and
  both messages come back, if at all, as a measured step of their own.)
* Plain-text answer between BEGIN and END lines carrying the request's random token; the last such
  block is read. A "\ No newline at end of ..." note is accepted only as its last line (no final
  newline); anywhere else it rejects the answer.
* Validation as synthmerge's: the answer starts and ends with the context lines (blank lines at a
  boundary are given back, up to n-2 of them, n the context lines at that boundary), holds both,
  what is between them ends with a newline and does not read as a patch; plus our marker rule and
  size bound.
* An answer equal to OURS is kept: synthmerge's default (it refuses one only on request, and its
  bench does not), and the report counts how many answers equal OURS anyway, per file and per
  region (synthmerge's no-change check is per region).
* No retry in R1 (faithful: synthmerge's retries shrink the context).
* Each model call has its own bound (call_timeout, 120 s), within the file's bound, which holds
  max_regions such calls (2400 s in the run's configuration; R0 had 180 s for all the regions of a
  file), so that a file with many regions does not fail for want of time.
* Budget: the provider's transport retries of a call share its one booking, so a 5xx answer that
  was billed and then retried can be billed twice while booked once (rare). To fix before
  production: book every attempt.

PM's additions:
1. A file is resolved only when every region is: the first rejected region rejects the file, which
   goes to the agent without a proposal; the report counts rejections by their first reason.
2. At most 20 regions per file (`max_regions`); a file with more makes no call and is reported as
   not sent (regions). Every call books its worst case against the budget first.
3. The answer equal to OURS: see above.
4. Deviation (a): the commit diff of THEIRS is not sent; it joins P5 (a protocol field), measured as
   its own step later.
5. No retry in R1.
6. Unit tests use recorded answers and fake servers only, never the network.
7. The run script reads the account's credits before every run and refuses to start unless the
   balance covers the cap plus $0.50; the key is read from its file and never printed.

Deviations, measured or stated:
* (b) The PATCH is our LCS line diff, not git's histogram diff: on the 300 paired cases (288
  content conflicts, 403 regions) the two differ in 18 regions of 17 cases (`bench histogram`).
* (c) Our worked example is a different snippet, written by us.
* (d) One model and no consensus, as Federico decided.
* (e) Leniency: a region at the end of a file may end without a newline when neither side has a
  final newline; synthmerge requires the newline.

**R1 result and addendum** (r1-20261019-101514, real spend $0.099): strict error 41.8% (R0 on the
same 300, re-judged: 53.8%); both accepted, wrong->correct 37 and correct->wrong 13, McNemar
p = 0.0009; the bound with reorder and whitespace counted correct 32.9%; human-outside 29 cases,
rate without them 33.2%. Of the 42 anchor rejections, 22 were CRLF files, and no CRLF file was
accepted: the context went to the model with CR and had to come back byte for byte, which models
do not do. Our bug, fixed before R2: a CRLF file is sent as LF and its answer converted back
(mixed line endings are sent as they are, and the report counts the cases by line endings). The
bench keeps the raw text of every rejected answer (<id>.rejected.txt beside the answers, 0600), so
that rejections can be classified. R1b, R1 with the fix, is the baseline of R2.

**Measurement.** Luna, effort low, max_tokens 8000, routing pinned to the standard tiers, the same
300 paired cases (`--sample 300 --seed 1`, the first 300 of the Luna 600), cap $1.20 a run, the
whole series under $5 real, and a run over $1.00 real stops the series until the PM is told.
* R0: the stored Luna answers on those 300, re-judged ($0).
* R1: the structure above.
* Then one extra at a time, each kept only if it helps on the same cases (lower strict error rate,
  more wrong-to-correct than correct-to-wrong flips): R2 one repair round with the same model and a
  duplicate-declaration check (Shen 2026's generate-validate-retry), R3 the enclosing scope of the
  region, R4 temperature 0, R5 the best so far at effort medium. P5 (the commit diff) and P6b (the
  commit subjects of the regions) need protocol fields and wait for their own approval.
* Every report gives, per language: the strict error rate, the bound with reordering and
  whitespace counted as correct, the cases where the human changed lines outside the conflict
  regions as their own category and a rate without them, acceptance and rejections by reason, the
  flips against the previous step and R0 with an exact McNemar p-value, and the spend.

## 14. The agent always resolves; the server's AI only proposes (Federico, PM-approved design)

**Final ruling (Federico, 2026-10-19, relayed by the PM; it supersedes the amendment at the end of
this section and every statement below that the server never lands a model's resolution).** An AI
resolution lands only through the operator's verification (15.9).

* Server AI configured and verify.command configured: the server resolves. If every deterministic
  check passes and verify.command passes on the merged tree, it LANDS, signed Graft-Resolved-By:
  <the resolver> and Graft-Checked-By: verify:<command>. Otherwise it goes to the agent as a
  PROPOSAL, with the verify output when verify failed.
* Server AI configured, no verify.command: proposal only, and the agent decides. An unverified AI
  resolution never lands.
* No AI: the agent resolves.
* Rules (trivial, union, rerere, command) are unchanged: they land after verify.command when one is
  configured, in the lane, as today.
* The composition report (15.3) stays a report, not a gate.
* One rule, in decideLand: a file that proposes may land only if verify.command ran and passed on
  the exact tree that lands, compared by tree object ID. No other code path lands a model's
  resolution.
* The trailer names what ran: Graft-Checked-By: verify:<command>. The syntax and operator checks
  run inside the resolver and are not separate trailers; the composition report is not built, so
  the trailer does not claim it.

This supersedes "AI lands with trace only". Before the ruling above, the server never landed a
resolution of the llm strategy by itself: it was a proposal, which the agent reviews, edits or
replaces, and lands with its own resolution (UserLandRequest.resolution, Graft-Resolved-By: agent/<actor>). The deterministic
strategies that the operator configures (trivial, union, rerere, command) are rules, not AI: a
resolution made by rules alone still lands. The llm auto-land path is removed, not kept behind a
flag.

* **Protocol (additive).** A ResponseFile gains `"proposal": true`; graft-resolve sets it on every
  file whose result came from the llm strategy, and only there. The server treats an answer with any
  proposal file as not landable. *The name-based fallback this item had (an answer whose resolver
  names the llm strategy) is removed by the review of f404a71: it read only the first "x/" of a name,
  so a list joined over rounds, a list cut to ",+N" or another vendor's name missed it. Replaced by
  the fail-safe origin of 15.9.*
* **Server.** With resolve_conflicts, a resolution by rules alone lands as today. When a file is a
  proposal, the land fails with the LandConflict package: proposal files get OUTCOME_PROPOSED,
  files that rules resolved keep OUTCOME_RESOLVED, and nothing lands partially. Every file with
  content carries `LandConflictFile.proposal` (OID, mode, action, strategy, and the explanation cut
  to 512 bytes), so that the agent can re-land rule results unchanged. No llm trailer exists any
  more; the agent's re-land records the agent.
* **Delivery: a hidden reference.** On a failed land with proposals the server writes the proposed
  blobs and one commit (its tree the proposals by path, its message the tip, the commit and the
  strategies), written into the repository itself (the land's quarantine is discarded), and updates
  `refs/graft/proposals/<first 16 hex of sha256(branch)>/<commit>` through the same reference
  transaction a land uses. One reference per (branch, landed commit), replaced by every failed land
  of that commit. On every land of a branch that asks for or brings a resolution (a plain land pays
  nothing; only those lands write proposals) the server deletes the branch's proposal references
  older than `proposal_ttl` (default 7 days) and keeps at most 20 per branch; the agent's land of
  the change drops its proposal. When the proposals cannot be stored the package still goes to
  the agent, each file of the answer saying "proposal not stored: <cause>" (logged and counted):
  the proposals are an aid, the package is the contract (review ruling M2).
* **(a) Authoritative storage mode (checked).** UserLand updates references through the
  reference-transaction hook (STORAGE.md, the voter): the proposal reference and its expiry
  deletions go through it like the branch, so in authoritative mode each is a changeset committed
  with the database compare-and-swap, with the pack of its objects uploaded before. They are not
  changelog entries: `changelog.ParseUpdates` and `corestore.IsChangelogRef` admit only refs/heads/
  and refs/tags/. The objects reach other nodes with the changeset pack, so the agent can read them
  on any node.
* **(b) Visibility and access (checked).** refs/graft/ is not hidden today:
  `git.InternalRefPrefixes` lists refs/environments, refs/keep-around, refs/merge-requests,
  refs/pipelines, refs/remotes and refs/tmp only. `refs/graft/proposals/` is added as an
  InternalReferenceTypeHidden prefix, so receive-pack hides it (no client can create, update or
  delete it by a push) and upload-pack does not advertise it. Agents cannot write references
  directly (WriteRef, UpdateReferences and DeleteRefs are never allowed for agents,
  AGENT-SECURITY.md). The agent reads the proposal with ObjectService.GetObjects, which agents may
  call and which reads objects by OID with git cat-file, without a reachability check. Observation,
  left as it is: graft-verify's refs/graft/green/ is not hidden either (advertised and pushable).
* **CLI and SDK.** `graft land --conflicts-dir DIR` writes each proposal to DIR/proposals/<path> (a
  proposal that cannot be read is reported and fails nothing else) and prints which files have one;
  `--resolved` is unchanged, so a proposal copied into DIR/files lands as the agent's. The SDK's
  ConflictSides gains the proposal (content, strategy, explanation), and ResolutionFromProposals
  starts a resolution from them (ResolutionFor is unchanged).
* **The PM's four conditions (implemented).**
  1. One writer, one prefix: landUpdateProposalRefs is the only function that writes these
     references. It refuses any name but refs/graft/proposals/<16 hex>/<object ID> before git
     runs, and updates as the server (no request identity), so the agent's policy does not judge
     the server's write. It is called only by landWriteProposals and the pruning.
  2. refs/graft/** is reserved: the reference-transaction hook refuses any update under it by an
     agent identity, whatever the policy says and even with no policy, so a push, UpdateReferences,
     DeleteRefs, WriteRef or an operation RPC fails. CheckAgentPolicy refuses it early too.
     graft-verify writes refs/graft/green/ with the shared token, which is not an agent.
  3. Hidden: refs/graft/proposals/ is hidden from advertisement and push. Two points are not
     covered, stated here: graft-verify's refs/graft/green/ is still advertised (hiding it changes
     graft-verify's users and needs its own decision), and an agent's read accessors (ListRefs,
     FindRefsByOID and the like) still list the names and OIDs of refs/graft/ references (no
     content; the blobs are read by OID as before). Filtering those is a change of the ref service
     for agents, proposed as its own step.
  4. Trace: the proposals' commit carries `Requested-By: <actor>` (the agent, or the user ID of the
     request) and the resolver; the server logs the reference, the actor and the resolver. The
     update itself is the server's.
  An end-to-end test runs the real graft-resolve with a model behind the command provider, under the
  example policy that gives the agent no refs/graft/ rule: the server proposes, the SDK reads the
  proposal, the agent lands it as its own (Graft-Resolved-By: agent/<actor>), and the proposal
  reference is dropped.
* **Review of e33a723 + b54b529 (PM rulings), implemented.**
  * M1, condition 3: refs/graft/** is internal for agents. One rule (internal/graft/agentrefs),
    applied once per transport: the agent guard of the gRPC server refuses a request whose
    revision, reference or pattern names refs/graft/ (the short forms graft/proposals/... and
    graft/green/..., which git completes to the server's namespaces, included; other graft/...
    short names are branches, the workspace's temporary branches graft/<agent>/... among them,
    which the first, broader rule refused and the suite caught), and removes such references
    from every response; git
    upload-pack and receive-pack hide the prefix from agents. A table-driven test classifies every
    string or bytes field of every RPC agents may call by default (a new one fails it); RPCs added
    with agent_rpcs are checked by field name. graft-verify's refs/graft/green/ stays advertised
    to other callers: VERIFY.md documents it for consumers (the newest tested tip), which are not
    agents; agents do not see it.
  * M2: a proposal that cannot be stored no longer turns the conflict into Internal: the package
    comes without it, each file of the answer saying "proposal not stored: <cause>", logged and
    counted (graft_land_proposal_write_errors_total).
  * M3: the proposals' commit is dated by the server's clock, never the request's timestamp; the
    housekeeping pass prunes the proposals of every branch (WithGraftProposalTTL), so idle and
    deleted branches expire, except in a repository housekept in a WAL transaction, where a
    reference update would bypass the transaction (logged; those expire through their branch's
    lands); an agent the policy does not let update the branch gets PermissionDenied before
    anything is resolved, and no proposal is kept.
  * m1: every deletion expects the target it read (internal/graft/proposals, the one writer and
    the one pruner, used by UserLand and housekeeping). m2: decideLand is the one decision point,
    with the guard "a file that proposes is never landed"; that is where a future gate lives.
    m4: the Resolver and Requested-By values pass landTrailerValueValid, and are quoted
    otherwise (the user ID is the caller's). m5: the e2e test names the llm strategy "model", so
    the flag, not the name fallback, makes the proposal; the release note (the PM's file) says
    that rerere entries recorded from llm answers before b54b529 must be cleared.
  * m6, measured: writing the proposals adds a median of 97 ms (min 69, p90 113, max 134 ms on the
    test machine) to the lane's hold; the resolver's own call, already in the lane for
    resolve_conflicts, takes seconds. Moving both out of the lane, like the precheck of an agent's
    resolution, is a step of its own.

* **Amendment: Federico's product decision (2026-10-19, relayed by the PM). Superseded by the final
  ruling at the top of this section: the gate is verify.command, not the composition check.** "When the server AI
  is configured, the server always tries to resolve, and it LANDS its resolution by itself ONLY if
  the proposal passes every deterministic check: syntax, anchors, no markers, and the composition
  check from §15 (keeps both changes, no duplicated both-added lines). There is still no
  self-declared confidence. If any check fails, the resolution goes to the agent as a proposal,
  with its report. If no AI is configured, the agent always resolves."
  * Gated on §15: until the composition check exists and is measured, a model's resolution stays a
    proposal, as implemented above. The composition check is what turns a proposal into a landable
    resolution; the rule "the server never lands llm output" becomes "the server lands llm output
    only when the composition check passed in this process", in one function, with tests.
  * Before it is enabled: the coverage (how many land by themselves) and the precision (how many of
    those are wrong), per language with a confidence interval, on the 300 cases; the default goes on
    only with Federico's OK after he sees them.
  * An AI-landed merge is signed Graft-Resolved-By: llm/<model> and Graft-Checked-By:
    syntax,composition, and the trace records the proposal and the report.
  * Everything above still applies: a resolution that fails a check is a proposal.

## 15. gitai's resolver design

**Principle.** A conflict is two changes to one base. Only the server holds the base, both
histories, the landing commit and the agent's request, so the server is the place to check what
it can check mechanically, and the agent, who decides, gets a proposal with an honest report of
what was checked, not a bare guess.

The four diagrams below are the source of truth of the flow. A code path that is in no diagram is a
patch and is removed; a commit that changes the flow changes the diagrams in the same commit, and
the reviewer checks the code against them with the table that follows. (planned) marks what does
not exist yet; (not enabled) marks what is designed and measured but switched off.

### 15.1 End to end

```
 agent: UserLand(branch, commit[, resolve_conflicts | resolution])
   |
   |-- resolution given? --yes--> PRECHECK, before the lane, in a discarded quarantine
   |                               branch moved / change already landed -> skipped (lane answers)
   |                               review: exact paths, modes, markers, pointer lines, size
   |                               resolver check (syntax + operator checks), bounded slots
   |                               VERIFY (below) the exact tree the resolution would land: fails ->
   |                               PACKAGE (every file REJECTED, the output); not in time -> PACKAGE
   |
   |-- resolve_conflicts? --yes--> PRE-RESOLVE, before the lane, in a discarded quarantine (15.8)
   |                               tip T read as the lane reads it; no branch / already landed /
   |                               the merge of T and the change is clean -> skipped (lane answers)
   |                               agent may not update the branch (policy) -> PermissionDenied
   |                               the conflicting files not yet resolved -> one VERDICT per file
   |                               (15.2), each kept by its key (path, base, ours, theirs, modes),
   |                               resolve slots; VERIFY (the one verify box, 15.9): when verify.command
   |                               is configured, or the answer holds a model's resolution, it runs on
   |                               the exact tree that would land, with a slot of the verify pool
   |                               (verify_concurrency, verify_per_repository); decideLand (15.4):
   |                                 not landable -> proposals kept (14) and PACKAGE now: the lane
   |                                                 is not entered; a model's files are PROPOSED,
   |                                                 a rule's REJECTED, with the verify output when
   |                                                 verify failed; NOT_ATTEMPTED (a rule's) or PROPOSED
   |                                                 (a model's) with the reason when the deadline cut it
   |                                 landable ----> the verdicts, and the verified tree, go to the lane
   v
 LANE (one per branch)
   |-- a resolution -----> landWithResolution: tip moved -> refuse, fresh PACKAGE
   |                         applies the precheck's verdicts; any failure -> PACKAGE
   |                         the tree it would write is the tree the precheck verified, else PACKAGE
   |                         (the lane runs no command) -> LAND as agent/<actor> (+ Graft-Checked-By)
   v
 3-way merge (rebase or merge) of the tip and the change
   |-- clean -------------------------------------------------------------> LAND
   |-- conflicts, no resolve_conflicts ------------------------------------> merge_conflict
   |-- conflicts, every conflicting file has a verdict with its key, and the lane's
   |   classification of the file agrees with the verdict's
   |       decideLand (15.4) on the tree the lane would write: with verify.enabled set, any
   |                     resolution (a rule's, a model's) LANDS only if that tree is the tree
   |                     verify.command passed on (tree object IDs), and a model's on no other tree,
   |                     configured or not. The lane runs no verification of any resolution. Merge
   |                     commit signed Graft-Resolved-By: <resolver> + Graft-Checked-By: verify:<command>.
   |                     Another tree (the tip moved) -> NEEDS VERIFICATION, below
   |-- conflicts, a file without such a verdict (new, or its sides changed)
   |       -> NEEDS RESOLUTION, out of the lane: PRE-RESOLVE only those files against the new tip,
   |          keeping the other verdicts, and enter the lane again
   |-- an answer whose tree is not the verified one (the tip moved)
           -> NEEDS VERIFICATION, out of the lane: rebuild the tree from the same verdicts (the
              resolver is called only for files that changed), VERIFY it, enter the lane again
 The rounds are bounded by the deadline of the land (the resolver's timeout, plus verify.timeout when
 verify.enabled is set) and by resolve_rounds (default 32, a safety cap, no contract). At
 the deadline: the final pass (15.9): the merge conflict with its PACKAGE, the model's files PROPOSED
 ("could not verify before the branch moved again"), the rules' files RESOLVED (NOT_ATTEMPTED with the
 reason when verify.enabled is set and their tree could not be verified), the files that changed
 under the land NOT_ATTEMPTED with the reason (a verified AI land is opportunistic: losing the race
 is harmless, the agent gets the proposal).
 The resolver is never called inside the lane, and neither is the verification of a model's resolution.

 PACKAGE to the agent (merge_conflict detail): sides by OID, hunks, per-file outcome + reason,
   per-file proposal (+ its report, in its explanation), the proposals kept under refs/graft/proposals/ (14),
   or, when they cannot be stored, "proposal not stored: <cause>" on each file
     -> the agent resolves (takes, edits or replaces the proposals) and re-lands with resolution
 every request and response of an agent: the refs/graft/ visibility filter (agentrefs);
 git upload-pack / receive-pack of an agent: refs/graft/ hidden
```

### 15.2 Per file

```
 conflicting file
   |
   v
 classify: path policy, shape, size, binary, lockfile, generated, minified
   |-- not sendable ----------------------------------------> AGENT (NOT_SENT, reason)
   v
 operator rules in their order (trivial, union, rerere, command), each with the syntax check and
 the operator's checks
   |-- a rule resolved it ---------------------------------> VERDICT RULE (lands, 15.4)
   v
 AI configured? --no-------------------------------------> AGENT (FAILED: no strategy applied)
   | yes
   v
 split into regions (3-way line diff); regions only one side changed are merged locally
   |-- more than max_regions (20) ------------------------> AGENT (NOT_SENT: regions)
   v
 for each region, one call (15.10): PATCH = base->theirs (3 context lines), OURS CHANGE = base->ours
 (symmetric context, capped), CODE = ours (3 anchors), THEIRS' and OURS' commit messages (capped);
 the answer is the last block: patched code, or ABSTAIN with a reason
   |-- ABSTAIN ----------------------------------------------> AGENT (abstained: the model's reason;
   |                                                            no proposal, no guess)
   v
 per-region checks: anchors, newline rule, not a patch, size, markers; composition of the region
 (15.3): drops and duplicates
   |-- a check finds something, and the region was not answered again -> ONE REPAIR ROUND: the same
   |       request, the previous answer and the findings quoted exactly; the new answer replaces the
   |       first if it passes the checks and has fewer findings; a second rejection rejects the region
   |-- any region fails -------------------------------------> AGENT (REJECTED, reason; no proposal:
   |                                                            all or nothing)
   v
 assemble the file; syntax of the file (tree-sitter): the first error is mapped to its region and, if
 that region was not answered again, quoted back in the same one repair round
 file checks: markers, tree-sitter syntax, operator checks
   |-- a check fails ----------------------------------------> AGENT (REJECTED, reason)
   v
 composition report (15.3) of the regions (a report, not a gate)
   v
 VERDICT PROPOSAL (PROPOSED, the proposal + its report)
   v
 verify.enabled set? --no--> stays a PROPOSAL: the agent decides ("no verify.command")
   | yes: the exact tree that would land is verified, out of the lane (15.9)
   |-- fails ------------------------------------------------> stays a PROPOSAL (+ the verify output)
   |-- not verified before the branch moved again -----------> stays a PROPOSAL (+ the reason)
   v
 passes --> LANDS, if it is still the tree to land (decideLand, 15.4), signed Graft-Checked-By: verify:...
```

### 15.3 Composition report (a report, not a gate)

```
 base B, ours O, theirs T, result R (normalized lines)
   |
   v
 diff3 regions of (B, O, T); does R keep every clean segment, in order?
   |-- no --------------------------------------------------> "changes lines outside the conflicts"
   v
 each region's slot in R; each side's hunks of base->side inside the region
   |
   v
 hunk the other side did not touch (uncontested):
   its added lines in the slot, its deleted lines gone --> "keeps <side>'s change at a-b"
   otherwise -------------------------------------------> "drops <side>'s change at base a-b"
 hunk whose base lines the other side also edited (contested):
   --------------------------------------------------------> "contested region resolved freely"
                                                              (nothing line-level to verify)
 a line both sides added, repeated more than either has it -> "duplicates a line both sides added"
 a line in none of B, O, T --------------------------------> "content in neither side" (warning)
```

### 15.4 The one decision point

```
 decideLand(answer, files, tree, verified): the pre-resolution calls it with the tree it verified,
 the lane with the tree it would write
   every file resolved by an operator rule ...... LAND, and with verify.enabled set only if it ran
                                                  and passed on THIS tree (tree object ID equality)
   a file a model resolved ...................... LAND only if verify.command ran and passed on THIS
                                                  tree: tree object ID equality, not the tip
   anything else ................................ PACKAGE (the model's files are proposals)
 One rule about a model's resolution. No other code path lands it. No check disables itself: a configuration that
 cannot be used stops the server at startup, and a failure at run time refuses the land. No
 confidence that the model declares about itself decides anything.
```

### 15.5 Box to code (the reviewer's audit table)

| diagram box | function | file |
|---|---|---|
| UserLand, PRECHECK | `UserLand`, `landPrecheckInQuarantine`, `landPrecheckResolution` | operations/land.go, land_reland.go |
| review of a resolution | `landReviewResolution`, `resolver.ValidateFile(WithSides)`, `landModeOfSides` | operations/land_reland.go, land_resolve.go; graft/resolver/validate.go |
| resolver check, bounded slots | `landCheckResolution`, `landCheckSlotted`, `Client.AcquireCheckSlot` | operations/land_reland.go; graft/resolver/check.go |
| LANE, re-land | `landAttempt`, `landingAttempt.decide`, `landWithResolution` | operations/land.go, land_attempt.go, land_reland.go |
| 3-way merge, listing | `landingAttempt.decide`, `landListConflicts` | operations/land_attempt.go, land_resolve.go |
| out of the lane: rounds, deadline | `landResolving`, `landPreResolve` | operations/land_preresolve.go |
| per-file verdicts, decide | `landResolveAt`, `landSetOutcomes`, `decideLand`, `landAnswerProposes`, `landKeyOf` | operations/land_resolve.go, land_proposals.go, land_preresolve.go |
| the tree that would land | `landResolvedTree` | operations/land_resolve.go |
| VERIFY, the one box, out of the lane, for every resolution | `landVerifyOutOfLane`, `landVerify`, `landPrecheckVerify` (an agent's), `VerifyPool.Acquire` | operations/land_verify.go, land_reland.go; graft/resolver/check.go |
| verdicts applied in the lane | `landApplyVerdicts`, `landNeedsResolution` | operations/land_resolve.go, land_preresolve.go |
| unverified in time: proposals | `landAnnotateUnverified`, `landStoppedReason`, `landFinalPass` | operations/land_resolve.go, land_preresolve.go |
| resolver call, bounded slots | `Client.AcquireResolveSlot`, `ResolveSlot.Resolve` | graft/resolver/check.go |
| policy before resolving | `hook.CheckAgentPolicy`, `landPolicyDeniedError` | operations/land_resolve.go, land_proposals.go |
| classify | `landClassify`, `resolver.SendRules` | operations/land_resolve.go; graft/resolver |
| operator rules, file checks | `Resolver.resolveFile`, `Resolver.runChecks`, `syntax.CheckMerge` | tools/graft-agents/resolve/resolve.go |
| AI per file | `llmStrategy.Resolve`, `ResolveLLM`, `llmResolvePatch` | tools/.../resolve/llm_strategy.go, llm_core.go, llm_patch.go |
| regions, assemble | `llmRegions`, `llmAssemble` | tools/.../resolve/llm_core.go |
| one call per region, its checks | `llmPatchSystem`, `llmPatchUser`, `llmPatchAnswer`, `llmCheckPatched`, `llmCheckAnswer` | tools/.../resolve/llm_patch.go, llm_core.go |
| proposal mark, no recording | `proposer`, `Resolver.resolveFile` | tools/.../resolve/llm_strategy.go, resolve.go |
| verify.command's checkout | `landCheckoutTree`, `landMaterializer.add`, `landCheckMaterialized`, `landRemoveCheckout`, `resolver.IsGitDirName` | operations/land_verify.go; graft/resolver/validate.go |
| merge commit, trailers | `landWriteResolution` | operations/land_resolve.go |
| PACKAGE | `landWithPackage`, `landListing.conflictPackage` | operations/land_package.go |
| proposals, hidden reference | `landWriteProposals`, `proposals.Update`, `proposals.Prune`, `landProposalsNotStored` | operations/land_proposals.go; graft/proposals |
| proposals' expiry in housekeeping | `RepositoryManager.pruneGraftProposals` | git/housekeeping/manager/optimize_repository.go |
| refs/graft/ visibility filter (agents) | `checkAgentRefs`, `filterAgentRefs`, `agentRefField`; `agentrefs.RevisionHidden` | gitaly/server/auth/agent_refs.go; graft/agentrefs |
| refs/graft/ hidden from agents' git | `hiddenUploadPackRefPrefixes`, `hiddenReceivePackRefPrefixes` | git/gitcmd/command_description.go |
| refs/graft/ reserved for the server | `graftReservedDenial` | gitaly/hook/referencetransaction.go |
| composition report | `llmCompose`, `llmReport`, `CompositionVerdict` (whole file, for the parity test), `landProposalExplanation` | tools/.../resolve/llm_compose.go, llm_compose_file.go, llm_patch.go; operations/land_proposals.go |
| symmetric context, abstain, repair | `llmPatchSystem`, `llmPatchUser`, `llmParseAnswer`, `llmRepairUser`, `llmResolvePatch` | tools/.../resolve/llm_patch.go |
| intent of OURS | `landOursMessage` | operations/land_resolve.go |

### 15.6 The composition check, measured: a report, not a gate

The design first meant the check to turn a proposal into a landable resolution (Federico's
amendment, 14): every edit of each side must appear in the result unless the other side's edit to
the same base lines supersedes it. Measured with zero model calls:

* **Definition, from the principle, frozen before measuring** (sha256 9396326b..., checked before
  every run). A hunk is contested when the other side edited overlapping base lines (or touches it,
  for an insertion); the result may compose a contested hunk freely, so it is never a drop. An
  uncontested hunk must be in the result. Duplicates and changes outside the conflicts fail.
  Disclosure: before freezing it, 8 human resolutions from the 300 had been looked at with an
  earlier, stricter prototype; the frozen definition is measured on cases outside the 300.
* **Human resolutions outside the 300** (2644 content cases): 82.6% pass; 324 change lines outside
  the conflicts, 103 duplicate, 32 drop.
* **Power.** Something verifiable (an uncontested hunk, or a line both sides added) exists in 20.0%
  of those cases and 19.2% of their regions (300: 29.9% of cases, 11.6% of regions). A diff3
  region exists because both sides edited overlapping base lines, so most regions are wholly
  contested and pass vacuously.
* **As a gate on R1b's accepted answers:** without it 143 of 244 correct, 58.6% [95% CI 52.3-64.6];
  with it, 135 of 220 passes correct, 61.4% [54.8-67.6]. 175 of the 220 passes are vacuous (64.6%
  correct); the verified passes are 48.9% correct [35.0-63.0]. If only passes landed, 73% of the
  conflicts would land and about 39% of those would be wrong. The gate does not move precision.
* **Contested regions have no line-level property.** A stricter prototype (every line either side
  added present unless superseded line by line) separated better on R1 (74.5% precision) but was
  built while looking at those cases, is still far below a landing bar (~90%), and fails half of the
  human merges (the right resolution of a contested statement is often a third version that holds
  neither side's line). Composition inside a contested region is semantic; a line-level check only
  measures overlap with the sides.

**Decisions.**
1. The AI stays proposal-only unless verify.command passes on the tree (the final ruling, 14 and 15.9).
2. Federico's amendment (AI landing when every deterministic check passes) is superseded by the
   final ruling: the gate is the operator's verify.command, because no deterministic check reaches
   the bar.
3. The composition report goes with every proposal, honest about what it can say: "keeps every
   uncontested change of both sides" or "drops THEIRS' change at base 40-44", "duplicates a line
   both sides added", "changes lines outside the conflicts", "N contested regions resolved freely".
   The duplicates flag carries signal (7 of 19 correct where it fires, against 58.6% overall): it is
   useful in the report and as a repair trigger.
4. The repair round (when approved) is driven only by deterministic findings with their exact
   wording: anchors, syntax, and the composition report's drops and duplicates where they apply.
5. The design work goes to what helps the agent decide: the report, symmetric context (each side's
   change from the base and its intent), and abstaining (the model may answer that the changes do
   not compose, which reaches the agent as abstained with the reason).

**Java.** R1b's Java answers are 54 of 54 correct (Wilson 95% CI 93.4-100). That is a property of the
model on these cases, not of the check, and most of them come from one repository (spring-boot).
It is a candidate for a per-language, measured policy, which would first need validation on Java
cases from other repositories.

### 15.7 Other resolvers, compared

From the PM's survey (17 tools and papers; prompts read from source where the licence allowed).
GitButler's AI resolve is described here from the PM's account of it; its code was not read.

* GitButler sends one structured JSON request per commit with every hunk; we send one call per
  region. Our own R0 (whole-file JSON) against R1 (per region) on the same 300 cases: 53.8% against
  41.8% strict error, and R0 -> R1b significantly more correct (McNemar p = 0.018). Per region stays.
* GitButler writes each resolution into both sides and lets git merge again, which keeps the
  non-conflicting changes; our llmAssemble rebuilds the file from the base, the clean regions and
  the resolved ones. The two are equivalent when the regions are those of git's own merge; to be
  adopted as an assertion test of llmAssemble (planned): writing the result into both sides and
  merging must give the assembled file.
* GitButler lands directly with no semantic check; ours proposes, with a report. That, and the
  measured finding that a line-level check cannot gate, is where gitai differs.
* Credit: the patch-application structure of R1 is synthmerge's (Andrea Arcangeli; GPL, structure
  only, our own wording and example). The evidence on scope (ConflictAgent, Shen 2026, MIT; Rover,
  CC BY) and on intent (GitWand, Copilot, MIT) informed the planned symmetric context. The design,
  the composition report and its measurement are gitai's.

### 15.8 Design delta: the resolution goes out of the lane (m6, PM-approved)

Today a land with resolve_conflicts calls the resolver inside the branch's lane, so the branch is
held for the resolver's whole call: seconds, minutes for a file with 20 regions at ~13 s each, and
~97 ms more to keep the proposals. A first version of this delta bound the resolution to the tip
read before the lane; on a busy branch (the 50-agent race) the tip moves every few hundred
milliseconds while a resolution takes seconds, so that version would fail exactly under load.
The resolution is bound to the files instead (the PM's correction):

* **What a file's verdict depends on.** The resolver answers a file from its sides: its key is
  (path, base OID, ours OID, theirs OID, the three modes). Two inputs come from elsewhere, and the
  design states them:
  * The classification (15.2) also reads the tip: the linguist-generated attribute from the tip's
    .gitattributes, and the paths a side comes from in the tip's tree (renames). The lane therefore
    runs the classification again on every file it reuses: local git work, bounded like the
    package's, never the resolver. A file whose classification at the lane's tip differs from its
    verdict's counts as changed.
  * A request carries the files that still need a verdict, and which files travel together depends
    on the round: after a tip moved, only the files whose sides changed are sent, alone. A resolver
    can therefore not rely on seeing the other conflicting files of the land, and a verdict must
    depend on its file only (its path, its sides and modes, the change's message, the
    configuration). A third-party resolver (the command or the HTTP service) must
    respect that contract; one that reads the tip, another file or the history of the repository
    through its own means would have its answer reused where it no longer holds. The risk is
    bounded: an answer of a model never lands unverified (the final ruling, 14 and 15.9), and the
    operator's rules are the resolver's own.
  * The request no longer carries the tip, its merge base or its commit message: the fields
    `tip`, `merge_base` and `messages.tip` are always empty, kept in the protocol for
    compatibility (a test asserts it). A verdict reused at another tip is then correct by
    construction: no resolver, the operator's command strategy included, can read the tip.
    RESOLVE.md states that an answer must depend on the file only. One exception was made later
    (15.10): `ours_message`, the intent of OURS for a file, a function of the tip's history of the
    path. It is a hint and not an input of the verdict: it is not part of the key (the sides are),
    and a resolver must not decide a verdict on it, only the quality of a proposal.
* **Pre-resolve before the lane.** UserLand reads the tip T as the lane does and, in a quarantine
  it discards, merges T and the change. Skipped (the lane answers as today) when the branch does
  not exist, the change already landed, or the merge is clean. Otherwise it runs the per-file flow
  of 15.2 for the conflicting files that have no verdict yet: the policy first, the
  classification, one resolver request for them, the validation; each verdict is kept by its key;
  then decideLand over all of them.
* **Not landable: no lane at all.** The land fails anyway, so the proposals are kept (and pruned,
  14) and the package is returned without entering the lane. Built per file, the package of an
  older tip stays true per file; the agent's re-land is bound to the tip of its resolution, as
  today (binding agents' resolutions to the files too is a possible later extension, not now).
* **Landable: applied in the lane, file by file.** The lane merges its own tip and the change. When
  every conflicting file has a landable verdict with its key, and the lane's classification of the
  file agrees, the lane writes the verdicts into the merge commit and runs no command (15.9); the
  resolver is not called. In a batch, a member's verdicts hold whenever their keys match; without
  verify.enabled the members chain, one on the other's tip. With it a member lands only on a tree
  that passed, so a member whose tree is not the verified one (the batch changed the tip under it) goes
  back out to be verified, and lands alone.
* **Needs resolution.** When a conflicting file has no verdict with its key (it is new, or a side
  changed), the lane returns the attempt with those files, without resolving. UserLand
  pre-resolves only them against the new tip, out of the lane, keeps every other verdict, and
  enters the lane again. A tip that moves on unrelated files costs no resolver call at all.
* **Progress, and its limit.** A round is lost only when the lane found a file whose sides
  changed, that is when another land on that file landed. So the lands that contend for a file
  all land, at least one per round, and N contenders need at most N rounds each. The rounds are
  therefore bounded by the resolver's timeout for the whole land only; resolve_rounds (default
  32) is a safety cap well above realistic contention, documented as a cap and not a contract
  (decided after the 12-concurrent graft-bench test failed with a cap of 3). The limit, stated:
  under continuous new arrivals on the same file, one land can keep losing until its deadline,
  and then ends with the merge conflict and its package (15.9, the final pass): the files that
  changed under it are NOT_ATTEMPTED with the reason "the files changed again before they could be
  resolved" (the old "files kept changing" message is gone, as it carried no package); past the cap
  the same.
* **Rules alike.** The operator rules' verdicts (trivial, union, rerere, command) are kept and
  reused by the same key, so the policy is the same for every strategy.
* **Slots: two pools.** check_concurrency bounds the checks of agents' resolutions and
  resolve_concurrency (new, default 2) the resolutions. A check is short and gates an agent's
  re-land; a resolution with a model takes seconds to minutes. One shared pool would let a few
  long resolutions starve the checks, and fairness inside one pool needs a scheduler for no gain
  over two plain semaphores; the memory bound of the process is the sum of the two pools' requests.
  Each request waits for its slot within the resolver's timeout and fails closed when it expires.
* **Nothing is verified in the lane any more.** This bullet said that verify.command still ran in the
  lane for the rules' resolutions and for agents'; it does not (15.9): every verification runs out
  of the lane, on one mechanism.
* **Measured (TestLandResolveBench, GRAFT_LAND_BENCH=1).** Six lands that the server resolves,
  all on the same file, and six plain lands that conflict with nothing, at once through the lanes
  of a standalone server, with a resolver that takes 500 ms; the same bench on the tree before
  and after, three runs each:

  | | before (resolver in the lane) | after (out of the lane) |
  |---|---|---|
  | lane held | 3.48-3.54 s | 0.72-0.93 s |
  | plain lands, p50 | 3.47-3.53 s | 151-205 ms |
  | resolve lands, p95 | 3.44-3.58 s | 3.21-3.22 s |
  | resolver calls | 6 | 11 |

  The plain lands no longer wait for the resolutions, and the lane is held for git work only.
  The cost is in resolver calls: every land on the same file loses a round to each land that
  lands before it, and resolves again. That is the worst case by construction (one file, every
  land on it); lands on different files, or a tip that moves on other files, cost no extra call.
* **Tests.**
  * The resolver is never called with the lane held: a resolver that blocks while another land of
    the branch proceeds.
  * A not-landable answer never enters the lane.
  * A tip that moves on an unrelated file reuses every verdict, with no second resolver call.
  * A tip that changes one conflicting file re-resolves only that file.
  * Files that keep changing fail at the deadline, or after resolve_rounds, with the reason
    naming them.
  * A batch member reuses its verdicts.
  * A high-churn test: many concurrent lands on the branch, the resolve lands among them land
    eventually, with no starvation; and with a short deadline, every land ends, landed or with
    its reason, and none hangs.
  * A proposal kept while a batch holds the lane has the published tip as parent (review of
    6090d5b, item 4): proposals are written before the lane only.
  * The two pools: a held resolve slot does not delay a check.

### 15.9 Design delta: a model's resolution lands through the verification (PM ruling)

The final ruling of 14: an AI resolution lands only through the operator's verification. The
verification runs the operator's tests, which take minutes, so it cannot run in the lane (15.8),
and the tree it verifies depends on the tip.

* **One mechanism, for every verification (PM ruling after the review of the checkout).** The
  verification that was left in the lane for the rules' resolutions and for agents' went out of it
  too: whatever made the resolution, verify.command runs on the exact tree, out of the lane, with a
  slot of the process's pool, and the lane compares tree object IDs. Why: in the lane it was bounded
  by nothing but one per branch, so the number of commands running in the process was unbounded;
  now every one goes through verify_concurrency and verify_per_repository, and the pool exists
  whenever verify.enabled is set, with or without an AI resolver. The costs, stated: a rules'
  land whose tip moved after its tree was verified is verified again (it is a model's land in this
  respect: liveness is the deadline's, and at the deadline the rules' files are NOT_ATTEMPTED and
  an agent's resolution gets "could not verify before the deadline of <t>" with its package); an
  agent's resolution is verified in its precheck, at the only tip the lane accepts it at. The
  per-process worst case of a verification, so that the operator can size it: inodes and disk at
  most verify_concurrency x (verify.max_tree_entries inodes, verify.max_tree_bytes of files), and
  that is the checkout alone: what the command itself writes (a build directory, a cache) is the
  operator's to bound, and counts against the same disk; heap at most verify_concurrency x (the
  paths kept, 128 MiB by the charge of the materializer; the entries and the target of no link);
  and a TMPDIR on tmpfs turns that disk into memory: point it at a disk, or lower the limits.
* **Verification out of the lane, on the exact tree.** For an answer that holds a model's
  resolution (or any answer, with verify.enabled set), and when nothing else keeps the land from landing (every file resolved or proposed),
  the pre-resolution builds the tree that would land (`landResolvedTree`: the merged tree of the
  tip and the change, the answer's files written in) and runs verify.command on it, in the
  quarantine, with a slot of the verify pool (`verify_concurrency`, default 2, a pool of its own:
  `graft_resolver_verify_slot_wait_seconds`). The tree object ID that passed is kept.
* **In the lane: tree equality.** The lane merges its own tip, applies the verdicts, builds the
  tree it would write, and asks decideLand about it: a model's resolution lands only if that tree is
  a tree that passed. Tree object IDs are compared, not tips: a tip that moved to the same tree
  keeps the verification; a tip that moved to another tree does not. The lane runs no verification
  of a model's resolution, and writes the merge commit with Graft-Checked-By: verify:<command>.
* **Tip moved: out again, no resolver call.** When the tree is not a verified one, the lane does not
  land. The land goes back out, rebuilds the tree from the same verdicts (the resolver is called only
  for files whose sides changed, as in 15.8), verifies the new tree, and enters the lane again. Each
  round costs one verification.
* **Liveness, stated plainly: the progress argument of 15.8 does not carry over.** In 15.8 a round is
  lost only when another land changed the file, so the lands that contend for a file all land. Here a
  round is lost when ANY land moved the branch, because any land changes the tree that was verified,
  whatever file it touched. On a busy branch a verified AI land therefore mostly ends as proposals
  within one verification (the deadline holds a verification's time and little more), and that is the
  designed, safe outcome: the agent gets the proposal and decides. A verified AI land is opportunistic.
  Two bounds keep it from costing more than that: a per-repository cap on the verifications at once
  (verify_per_repository, default 1, inside verify_concurrency), so that one busy repository cannot take
  the whole pool, and the deadline of the land. A land that waits for its repository's turn holds
  nothing of the pool, and a wait that outlasts the deadline ends in proposals like the rest.
* **Deadline: a proposal, not a failure.** The deadline of the land is the resolver's timeout plus
  verify.timeout when verify.enabled is set. A verified AI land is opportunistic: when the
  deadline ends before a tree could be verified and landed, the land does not fail for the agent. The
  model's files become PROPOSED with the reason "could not verify before the branch moved again" (or
  "before the deadline of <t>" when the verification itself was cut), the proposals are kept (14), and
  the package goes to the agent. This final pass verifies nothing and calls no resolver. It is the end
  of every land that had a conflict and could not go on, not only of a model's: the deadline wherever it
  fell (the listing, the tree, the resolution, the verification), the cap on the rounds, and a land that
  the lane or its batch kept handing back (a model's resolution whose batch ran out of rebuilds is
  handed back as a land that needs its new tree verified, not failed with "kept moving"). The files
  that have a verdict are reported with it (a model's PROPOSED, a rule's RESOLVED, or NOT_ATTEMPTED with
  the reason when verify.enabled is set and its tree could not be verified; their proposals kept); the files that changed under the land and have none are NOT_ATTEMPTED, with the reason. With
  nothing known yet it is the merge conflict with its package and no proposals, with the reason. A
  land that had a conflict never returns a bare deadline to the agent: only a caller that gives up
  gets its cancellation.
* **A verification that fails** (non-zero exit, or its own timeout) is a verdict, not a deadline: the
  files stay PROPOSED and carry "verification failed: exit status N: <the first 4 KiB of the
  output>" (the package cuts a reason to 512 bytes). Nothing is verified for a land that cannot land
  anyway (a file refused, a file left to the agent).
* **Rules are unchanged.** A resolution made by rules alone lands after verify.command, which runs in
  the lane, as before; so does an agent's resolution. Moving them onto the same out-of-lane mechanism
  is the next step, measured on its own, so that in the end there is one verification mechanism.
* **What is a model's resolution: the origin of each file, fail-safe.** Each resolved file carries
  `origin`, `rule` or `model` (the older `proposal: true` means `model` and is still read). A file
  is a rule's only when it says so; a file without an origin, or with an unknown one, is a model's.
  It lands without verification only if it is marked `rule`; otherwise decideLand's one rule
  applies. The name of the resolver decides nothing (a joined list, a list cut to ",+N", another
  vendor's name). The server normalizes the origin when the answer arrives, so the answers kept by
  their keys carry it and the rounds that reuse them never depend on a name. Third-party resolvers
  that do not set `origin` no longer land without the operator's verification: that is the change.
* **What the verification sees: the tree, byte for byte.** git-archive(1) honours the attributes of
  the tree itself: export-ignore hides a file from the command, and a .gitattributes written by an
  agent or by a model could therefore decide what the operator's tests see. The checkout is written
  from git-ls-tree(1) -r and git-cat-file(1) --batch, which apply no attribute (no export-*, eol,
  filter or smudge), with modes, symbolic links as links and gitlinks as empty directories, and is
  read back and compared with the listing, the content included (every file and link is hashed as a
  git blob and must be the OID of the tree); a difference fails the checkout closed. The same function
  serves the verification of rules and of agents' resolutions.
* **What the checkout refuses, and what it costs.** The operator's command may run git in the checkout,
  so a path component that names .git on any file system is refused, by the one rule the resolver
  already uses for the special files (resolver.IsGitDirName: case-insensitive, the code points HFS+
  ignores, NTFS "git~1" and trailing spaces and periods): a .git of the tree would be the repository
  of the command, and its configuration and hooks would run. Also refused: an entry under a symbolic
  link of the tree, also when a case-insensitive file system would see the same name (a link "A" and
  an entry "a/x"), a parent that is a link or a file, a path over 4096 bytes. Each directory is made
  one component at a time and looked at with Lstat (never MkdirAll, which follows links), a file is
  created with O_EXCL and O_NOFOLLOW. Resources, all failing closed with the reason: a git tree can
  name one subtree many times, so a push of a few KB expands to as many paths as it has names, each as
  deep as the subtree. verify.max_tree_entries (default 200000) counts the entries AND the directories
  that the checkout makes (both are inodes); verify.max_tree_depth (default 64) the components of a
  path; verify.max_tree_bytes (default 2 GiB) the bytes of the files and of the targets of links; and
  a fixed 128 MiB the memory of the paths, each entry and each directory charged twice (the sets that
  look for links and for folded names). The limits are checked entry by entry, from the listing
  (ls-tree -l), BEFORE that entry's content is read, not from a first pass over the whole listing: the
  listing and the blobs are streamed through two commands, and what stays in memory is exactly what
  the materializer charges against the fixed 128 MiB budget (landMaxPathBytes): each entry its path
  plus 256 bytes, each distinct prefix twice its length plus 64 (the folded key and the spelling),
  each directory made its path plus 64, each link its path plus 64, and nothing of the target of a link
  or the content of a file (the read-back hashes what is on disk). A link target is at most 4095
  bytes, refused up front. An entry that would go over a limit stops the checkout with the earlier ones
  already written. The context is checked at every entry. A name over 255 bytes, a path that with the
  directory reaches PATH_MAX, a NUL in the target of a link, and a system call that fails (a name
  that exists already, no room) are refused with the reason, not errors of the server. The checkout
  runs inside verify.timeout, as the command does. A refusal is no error of the server: nothing is
  verified, the files are PROPOSED (a model's) or REJECTED (rules'), and the reason says which limit
  or path. Not built: NTFS trailing-dot and space folding in the check of colliding names (the
  .git check has it); Gitaly does not run on Windows. Under MVCC (a snapshot pinned for the RPC) a
  lost race ends with the retryable FailedPrecondition "kept moving" even for a model's resolution:
  that call carries no package; the client's retry gets one.
* **Tests.** A model's resolution that passes lands, signed, with the command run once (so not in
  the lane); one that fails gives a proposal and the output; without a verify.command it is a
  proposal; a tip that moved gets its new tree verified and lands, with no resolver call; a tip that
  moved to the same tree keeps the verification; churn past the deadline gives a proposal, and so does
  the cap on the rounds; the lane is free while a verification runs (a plain land passes through it);
  the lane never verifies; the verify pool bounds the verifications at once, those of rules and of
  agents' resolutions as well; and the table test of decideLand, whose one rule is compared by tree
  object ID.

### 15.10 Design step: symmetric context, abstaining, one deterministic repair, the report

The design work of 15.6 (decision 5), as ONE change measured as one step against R1b on the same 300
cases. Each element has an option to switch it off, so that an ablation removes one element and the
step with all off is R1b byte for byte (the request of every test that pins R1b runs with all three
off). All are on by default until the measurement says otherwise.

* **The regions are git's** (`git_regions`, default on). The R1b run took its regions from our own
  line diff, which disagreed with git's merge in 18 of the 244 accepted cases of the benchmark: in 13
  it found no conflict where git sees one and merged without the model (7 of those 13 are wrong). The
  regions now come from `git merge-file --diff3` on the three versions, the engine of the server, in a
  private temporary directory with markers no line of the file can imitate (labels with a random
  token, a marker size longer than any run of marker characters at the start of a line). Our diff is
  the fallback when git cannot be run or a side lacks a final newline (a marker line begins a line).
  The report is attached on every path, also for a file that git merges without the model. The
  composition check, with these regions, agrees with the frozen prototype in 243 of 244 verdicts.
* **Symmetric context** (`symmetric_context`). Beside THEIRS' PATCH the request gets OURS' own change
  to the region (OURS CHANGE, the unified diff base -> the CODE with the same three lines of context)
  and the commit message of OURS (`FileConflict.ours_message`: the newest commit between the merge
  base and the tip that touched the path, `git log -1 base..tip -- path`, found by the server, cut to
  4 KiB; the bench finds it the same way from the case's merge base). OURS CHANGE and OURS' message
  are capped (`max_context_bytes`, default 4096: OURS CHANGE up to the cap, the message up to a
  quarter); THEIRS' message keeps the cap it had (4 KiB) with or without the option, so that the
  ablation of the symmetric context removes OURS' side and changes nothing of THEIRS'. The request
  says when it cut. The verdict of a file is still kept by its sides, not by
  this message: it is a function of the tip's history of the path, which OURS' blob stands for.
* **Abstaining** (`abstain`). The model may answer, instead of the patched code, an ABSTAIN block with
  one or two sentences on which lines contradict and why. The last block of either kind in the answer
  is the answer. One abstaining region abstains the file: no proposal, no guess; the attempt is
  `abstained` and its reason ("the model abstained on region 2: ...", one line, 400 bytes) goes to the
  agent in the package (outcome FAILED, the reason is the model's). The bench counts these apart.
* **One repair round, driven only by deterministic findings** (`repair`). A region is asked again at
  most once, and only for what a check found, quoted in its exact words after the same request and
  the model's previous answer: a rule of an answer that was broken (the anchors, the newline, a
  patch, markers, size, no block at all), a drop or a duplicate of the composition of the region
  (15.3), or the first syntax error of the assembled file, mapped to the region it is in (an error at
  the line of its answer or one line before or after). Nothing in a repair is the model's opinion of
  itself. The repaired answer replaces the first if it passes the rules and, for a composition
  finding, has fewer findings; a failed optional repair (a call that fails, a worse answer) keeps the
  first answer. A rule-breaking answer that stays broken after the round is rejected. A model that
  abstains in the round abstains. The calls and the repair rounds are counted.
* **The composition report** is no longer planned. For the answer of each region it words, from
  the definition frozen in 15.6: what the answer drops ("drops THEIRS' change at base line 6: the line
  "T6" it adds is missing"), what it repeats ("duplicates a line both sides added"), the contested
  changes it resolved freely, the lines found in neither side, and "keeps every uncontested change of
  both sides (n)" when nothing is wrong. It is a report and not a gate (the finding of 15.6 stands). It
  goes with the proposal: `ResolvedFile.report` in graft-resolve's protocol, `ResponseFile.report` in
  the server's, and the start of the proposal's explanation ("report: ...; ...", cut to the 512 bytes
  of a reason, so a long report is cut). "Changes lines outside the conflicts" cannot happen to an
  answer the strategy assembles from the base and the regions; `CompositionVerdict` checks a whole file
  for it. NTFS-style folding is not part of this check.
* **Parity with the prototype.** The check is a port of compose2.py with the regions of this package's
  regions (git's, as above), and `CompositionVerdict` is compared with the prototype's verdicts on the
  244 accepted answers of R1b (offline, `TestCompositionParity`, GRAFT_COMPOSE_PARITY): 243 agree
  (99.6%) and 1 differs (the prototype's drop against the port's pass). With the regions of our own
  line diff it was 226 of 230, and 14 had no region where git's merge sees a conflict.
* **Measurement.** One step against R1b (all three elements off) on the same 300 cases with Luna, then
  one ablation per element that removes it, only to justify that element. The result goes here when it
  is in.

### 15.11 Design step 2 (planned, nothing built): the strategy chooses first, the server assembles

Federico's idea, as the PM framed it: a model that WRITES a resolution can write a line nobody had, or drift
whitespace; a model that CHOOSES cannot. The same llm strategy gets a first step. This section fixes the
constraints and the pre-registered gate BEFORE any paid arm; it changes no product code, and nothing here
goes into the product without Federico's decision after the measurement.

* **Each part does what it is good at.** Git computes the facts (the regions, the sides, the base). A
  decision model, **Jev** (`typesafe/jev-1.14` through OpenRouter, TypeSafe's calibrated decision model; it
  cannot write code), DECIDES per region among `OURS`, `THEIRS`, `OURS+THEIRS`, `THEIRS+OURS`,
  `NEEDS-MERGE`, and returns a probability for every option. A language model WRITES only where Jev says
  NEEDS-MERGE (2-3 screened writers produce candidates; Jev then chooses among the distinct candidates of the
  region plus "none: send to the agent"). Deterministic checks and the verification decide landing. The
  single landing rule of `decideLand` is unchanged: a resolution lands only through the verification on the
  exact tree, the origin stays "model" (15.9), and nothing else lands.
* **One strategy, a first step.** If every region is a pick, the server assembles the file deterministically
  from the sides and the clean segments of the merge: the answer holds no line that no side has and no
  whitespace of a model's. If any region needs a merge, the file goes to today's writer path (15.10) and
  becomes a proposal as now.
* **How Jev is called** (probed 2026-10-19, two calls, $0.000022 each): `POST https://openrouter.ai/api/v1/systemone`
  with the OpenRouter key, body `{model, state, questions:{id:{type:"choice", instructions, criteria:{option:
  description}}}}`; the answer per question is `{choice, probabilities:{option:p}, confidence}` and `usage.cost`
  is in the response. One request per file: the state (path, per region base/ours/theirs lines with context,
  both commit messages) is ingested once and each region is one question. At most 255 options, 32 MiB; default
  context 64k per request and 32k per question (a header raises both to 262k, slower and less accurate, and is
  not used until a file needs it). The model is priced per input token only ($0.042 per million). Jev leans
  toward the FIRST option (TypeSafe's own jaggedness note, confirmed by the probe: the same question with the
  options reversed moved OURS+THEIRS from 0.69 to 0.54 and NEEDS-MERGE from 0.15 to 0.26), so every decision
  is asked in two orders and the probabilities are averaged; a decision whose argmax differs between the two
  orders is NEEDS-MERGE.
* **The gate is fixed before it is measured.** "Jev's averaged probability for the picked option is at least
  tau in every region of the file, no region is NEEDS-MERGE, plus the verification." (A cross-family
  agreement gate, "every region picked identically by all N models", is measured beside it.) tau, and N and
  the model set for the agreement gate, are tuned on half of a family-stratified sample of the corpus OUTSIDE
  the 300 (the corpus is 48% one repository, see below, and a plain sample would be tuned on that repository;
  spring-boot is capped in the sample) and then FROZEN; the other half and the 300 are the held-out
  confirmation, reported separately and pooled. The target of at most 5% wrong among what lands is reported as met
  only if the Wilson 95% upper bound of the wrong rate is at most 5%, under BOTH judges: the strict one (the
  human's file after normalization) and judge A (the reference-based LLM judge adapted from ConflictAgent's
  rubric, MIT, measured offline; it tolerates whitespace, order and equivalent phrasing). Under neither is
  the point estimate enough. That bound needs about 75 landed files with none wrong, 150 with one, 300 with
  three; the ceiling below says what a sample can reach. Everything is judged against the constant policy
  "always THEIRS" and reported with and without spring-boot.
* **Concatenations are measured, not assumed.** `OURS+THEIRS` and `THEIRS+OURS` stay in the vocabulary only if
  the data shows they are safe: their precision is reported separately from the single-side picks, and if it
  is clearly worse they are dropped from the vocabulary (those regions become NEEDS-MERGE), not special-cased.
* **The report applies to assembled files too.** A pick of one side can "drop the other side's change at base
  a-b" (15.3): the composition report of 15.10 is computed on the assembled file and goes to the agent with
  the proposal. It is honest information, not a gate.
* **Never sent stays never sent.** Lockfiles, generated, minified, binary and large files keep their paths
  (3.4); the chooser only replaces the writer's first step for files the llm strategy already receives.

**Free numbers that bound it** (`ceiling.py`, `ceiling2.py`; no model, no network; git's regions; the human's
resolution matched region by region against the four picks after the judge's normalization, groups of regions
with no clean line between them matched together). The mined corpus has 2,935 content files (plus 237
modify-delete / add-add files, not region-based):

| | files | every region a side-pick | of which concatenation only | "always THEIRS" whole file |
|---|---|---|---|---|
| corpus | 2935 | 1614 = 55.0% [53.2-56.8] | 179 | 1357 = 46.2% |
| corpus without spring-boot | 1518 | 316 = 20.8% | | 75 = 4.9% |
| the 300 | 288 | 118 = 41.0% [35.4-46.7] | | 72 = 25.0% |
| outside the 300 | 2647 | 1496 = 56.5% [54.6-58.4] | | 1285 = 48.5% |
| go / python / rust-java / ts-js (corpus) | 447 / 92 / 1440 / 956 | 21.9% / 37.0% / 90.4% / 18.8% | | 4.7% / 9.8% / 89.0% / 4.7% |

* The ceiling is **41% of the files of the 300 and 21% outside spring-boot**. spring-boot is 1,417 of the 2,935
  files, and there the human takes THEIRS in 90% of the files (why was not investigated); a
  constant policy "always THEIRS" is already right for 89% of the java files of the corpus and for
  almost nothing elsewhere (4.7% in go and ts-js). A chooser must therefore be measured **outside spring-boot
  and against this constant policy**, or its number is the repository's, not the model's.
* In go and ts-js a region is a pure side-pick in about 30% of the cases (go 30% of 628 regions, ts-js 32% of
  1,080); in 70% the human wrote something that is none of the four, and the file goes to the writer.
* The human's choice is **constant within a file** in 1,599 of the 1,614 all-pick files (99%): one pick for the
  whole file. A per-file pick would lose almost nothing against a per-region pick on the corpus; the
  per-region vocabulary is kept because the model sees regions, and the free number says it does not need more.
* Concatenations: 263 of the 1,933 side-pick regions of the corpus (14%) are `OURS+THEIRS` or `THEIRS+OURS`, but
  outside java they are the common case: go 85 of 187 (45%), ts-js 141 of 350 (40%), python 19 of 62 (31%), java
  18 of 1,334 (1%). 179 of the 1,614 all-pick files are all-pick only through one. Dropping the concatenations
  would cost most of the ceiling in go and ts-js, which is why their precision is measured, not assumed.
* Files whose human resolution changed lines outside the conflicts ("outside": 365 of the 2,935, 41 of the 288
  of the 300) cannot be assembled from the sides by any chooser: they are wrong for it by construction.
* By path: test files 71% all-pick, source 49%; java 94%, ts 20%, go 19%, js 8%, py 36%. The corpus has no lockfile and
  two generated-path files (those rules apply first).

**Measurement plan (each paid arm needs the PM's OK and has its own cap of $0.50, the chooser's total $1.50).**
The chooser prompt is our own wording. First on a family-stratified 100 outside the 300 (tuning), with Luna
and two cheap screened models; per pick and per file: precision under both judges, coverage, the share of
NEEDS-MERGE, agreement-gated precision (2 of 3 and 3 of 3 agree), and cost. Then once on the 300 for the
chosen gate. The result goes here when it is in.
