# Code review

Graft has a small code review system built into the server, designed for
agents as much as for people. A review is a change (a head commit) that
targets a branch. The server classifies every changed file (code, comment-only,
docs, tests, generated, config), and approval rules say how many approvals each
class needs. A fix to a comment does not need a new approval; a logic change
does.

Code: `internal/gitaly/service/review` (server), `internal/graft/review/store`
(storage), `internal/graft/review/classify` (classifier),
`internal/graft/review/rules` (rules engine), `sdk/review.go` (SDK),
`cmd/graft/review.go` (CLI). Protocol: `proto/review.proto`.

Phase 5a stops at review state: **landing does not check reviews yet.** The
"approved review required to land" gate and per-directory write permissions
are phase 5b.

## Concepts

- **Review**: `id` (a decimal counter per repository), `target_branch` (a
  branch name without `refs/heads/`), `base_commit_id`, `head_commit_id`,
  `author` (the identity that created it), `title`, `description`, `state`
  (open, landed, closed), `files`, `verdicts`, `comments`, `created_at`,
  `updated_at`, plus the computed `requirements` and `approved`.
- **Files**: every path changed between the base and the head
  (`git diff-tree -r -z --no-renames base head`), each with a class and a
  human-readable `reason`. A rename shows as a deletion and an addition.
- **Verdict**: an approval or a change request by one identity on one head
  commit. A verdict is `current` while it counts for the current head. Every
  verdict is kept, oldest first.
- **Comment**: a message by one identity, optionally anchored to a file of the
  review and a 1-based line, recorded with the head it was written on. Comment
  ids are their 1-based position in the review.
- **Identity**: the actor of a request is the identity of its token
  (`identity.Actor`), or `anonymous` without one. Rules and author checks only
  mean something when every user and agent has its own token.

## RPCs: ReviewService

| RPC | Type | What it does |
| --- | --- | --- |
| `CreateReview` | mutator | Opens a review of `head_commit_id` against `target_branch`. |
| `GetReview` | accessor | Returns one review with its requirements evaluated. |
| `ListReviews` | accessor, stream | Lists reviews, newest first. |
| `UpdateReview` | mutator | Moves the review to a new head and decides which verdicts stay current. |
| `AddReviewComment` | mutator | Adds a comment. |
| `SubmitReviewVerdict` | mutator | Records an approval or a change request on the current head. |
| `CloseReview` | mutator | Marks the review closed, or landed with `landed`. |

Every read (`GetReview`, `ListReviews`, and every mutator's response)
recomputes `requirements` and `approved` from the configured rules, so a change
of the rules applies to existing reviews at once.

### CreateReview

- `target_branch` must be a valid branch name and must exist (`NotFound`).
- `head_commit_id` must resolve to a commit (`NotFound` when missing,
  `InvalidArgument` for another object type).
- `base_commit_id` is optional. Empty means `git merge-base head <branch tip>`
  (`FailedPrecondition` when they have no common ancestor). An explicit base
  must be one of `git merge-base --all head <branch tip>`, otherwise
  `FailedPrecondition`: any other base would misstate the change (the head
  itself, or a commit with the head's tree, would hide every change, and an
  older common ancestor hides a change that reverts what the branch gained
  since).
- The author is the caller. The review starts open, with no verdicts.

### GetReview and ListReviews

`GetReview` takes an `id` (`InvalidArgument` when empty, `NotFound` when
unknown). `ListReviews` filters by `target_branch` (empty: all), lists only
open reviews unless `include_closed` is set, and returns at most `limit`
reviews (0 means 100), 50 per message. Listings carry neither comments nor
verdict bodies.

### UpdateReview

- **Only the author may move the head** (`PermissionDenied` otherwise). The
  rules exclude the author from approving their own change; that would mean
  nothing if another identity could put its own code under the author's name.
- The review must be open (`FailedPrecondition`).
- `expected_head_commit_id`, when set, must equal the current head
  (compare-and-swap, `FailedPrecondition`).
- The new head must descend from the review's base (`FailedPrecondition`):
  otherwise the files computed against the base would not be the change of the
  head.
- The same head as the current one is a no-op.
- The files are recomputed as base..new head, and each current verdict is
  re-evaluated by the carry-over rule below.

### AddReviewComment

Anyone may comment on an open review. The body must be non-empty and at most
64 KiB, `line` must not be negative, and `path`, when set, must be one of the
files of the review (`InvalidArgument`). The line is not checked against the
length of the file.

### SubmitReviewVerdict

Anyone may give a verdict on an open review, the author included (the author's
approval is recorded but only counts under a rule with `allow_author`).
`head_commit_id` is required and must equal the review's current head
(`FailedPrecondition` otherwise), so a verdict never lands on code the reviewer
did not see. The body is at most 64 KiB. Earlier verdicts are not rewritten: a
later verdict by the same identity supersedes the earlier one for counting.

### CloseReview

**Only the author may close a review**, as closed or with `landed` as landed
(`PermissionDenied` otherwise). A closed or landed review rejects every
further mutation with `FailedPrecondition`. `landed` is a label set by the
tool that landed the change; the server does not check that the head landed.

## The classifier

`classify.File(path, old, new)` returns one class and a reason. `old` or `new`
is nil for an added or deleted file. The checks run in this order and the
first match wins:

1. **generated**: the file name ends in `.pb.go`, `_pb2.py`, `.min.js` or
   `_generated.go`; or one of the first 20 lines of the **old** version
   contains `@generated`, or `Code generated` together with `DO NOT EDIT`; or
   the file is added with such a marker. A marker that a change adds to an
   existing file does not count, otherwise adding one would turn any logic
   change into a generated change.
2. **tests**: the name ends in `_test.go`, `_test.py`, `.spec.{js,ts,jsx,tsx}`
   or `.test.{js,ts,jsx,tsx}`, matches `test_*.py`, or a directory component is
   `test`, `tests`, `testdata` or `__tests__`.
3. **docs**: the extension is `.md`, `.rst`, `.adoc` or `.txt`, or a directory
   component is `doc` or `docs`. Build and dependency manifests with a `.txt`
   extension are never docs: `CMakeLists.txt`, any `*requirements*.txt` or
   `*constraints*.txt`, and any `.txt` under a `requirements/` directory.
4. **config**: `.toml`, `.yaml`, `.yml`, `.json`, `.ini`, `.cfg`,
   `.editorconfig`, `.gitignore`, `.gitattributes`.
5. **comment_only**: both versions exist, the language is supported, both
   versions lex with certainty, and their token streams with comments and
   blanks removed are identical.
6. **code**: everything else, including every uncertainty.

COMMENT_ONLY is only decided for four languages, and only when the lexer is
certain:

- **Go** (`.go`). Line breaks are tokens (automatic semicolons). Directive
  comments (`//go:`, `//export`, `//line`, `//+build`, `extern`) are code. A
  file that imports `"C"` (in any quoting) or has a `#cgo` line is code.
- **Python** (`.py`). Only `#` comments are dropped; docstrings and all strings
  are code. Indentation and line structure are tokens, and a line continuation
  backslash is a token. A `#!` comment, or any comment containing `coding`
  (an encoding declaration), makes the file code. So does an f-string or
  t-string with a replacement field, and a name directly followed by a quote
  that is not a string prefix.
- **Rust** (`.rs`). `//` and nested `/* */` comments are dropped, but doc
  comments (`///`, `//!`, `/** */`, `/*! */`) are code. Literal prefixes and
  raw strings are handled; `stringify!`, literal suffixes and a shebang line
  make the file code.
- **Java** (`.java`). `//` and `/* */` comments are dropped, but Javadoc
  `/** */` and `///` comments are code. Text blocks are strings. Any `\u`
  escape makes the file code.

Each language splits runs of operator characters with its own operator set,
the way its compiler does. Any other extension (C, C++, JavaScript,
TypeScript, Ruby, shell, ...) is code unless a path rule matches. Binary
content, invalid UTF-8, a lone carriage return, an unterminated string or
comment, or a source over 1 MiB is code.

The server adds three rules before the classifier runs: a change of the file
mode, a symlink and a submodule are code (their content does not show what
the change does), and a version larger than 1 MiB is not read and is code. A
blob that is missing from the object database is treated as an absent side
(as if the file were added or deleted), so such a file can only get a class
from a path rule or from a generated marker of the side that is present.

## The rules engine

`rules.Evaluate(rules, files, author, verdicts)` returns one requirement per
rule that applies to at least one file, in rule order, and `approved`.

- A rule applies to a file when the path matches one of its `paths` (or
  `paths` is empty) and the file's class is in its `classes` (or `classes` is
  empty).
- Only the **latest** verdict of each identity counts, and only when it is
  current. The latest verdict is chosen among all verdicts before currency is
  checked, so an approval superseded by a later change request never counts
  again, even if the change request later becomes non-current.
- An approval counts for a rule when its identity matches one of the rule's
  `approvers` (or `approvers` is empty) and it is not the author, unless the
  rule sets `allow_author`. Each identity counts once.
- A requirement is satisfied when its approvals reach `approvals`.
- The review is approved when every requirement is satisfied and no identity's
  latest current verdict is a change request. A review that no rule applies to
  (for example docs only, under the default rules) is approved without any
  approval.

Globs (for `paths` and `approvers`) have the syntax of the standalone policy:
`path.Match` patterns, where a trailing `/**` matches anything below that
directory at any depth, any other trailing `**` matches any remainder
including `/`, and a bare `**` matches everything. `*` alone does not cross a
`/`.

## The carry-over rule

When `UpdateReview` moves the head, each verdict that is still current is
checked against the change from **the commit that verdict was given on** to
the new head (not from the previous head, so several small updates are judged
as a whole). The verdict stays current only when every file in that change is:

- **comment_only**, or
- **docs** and documentation by its own name (`.md`, `.rst`, `.adoc`, `.txt`,
  not a build manifest). A file that is docs only because it sits under a
  `doc/` or `docs/` directory (`internal/doc/handler.go`) does not qualify.

An empty change qualifies. Otherwise the verdict becomes non-current, and it
never becomes current again. When the verdict's commit is no longer in the
object database (pruned after a force push), the change cannot be compared and
the verdict becomes non-current.

Since the files of the review are always base..head, a comment-only update of
a code file keeps the file's class as `code` (the review still changes code)
while the approvals given before the update stay current.

## Configuration: `[[graft.review.rule]]`

```toml
[[graft.review.rule]]
name = "code"                     # required; shown in requirements
classes = ["code", "config"]      # empty: every class
paths = ["internal/**", "cmd/**"] # empty: every file
approvals = 1                     # current approvals needed (>= 0)
approvers = ["team-*"]            # identity globs; empty: anyone
allow_author = false              # the author's approval counts

[[graft.review.rule]]
name = "security"
paths = ["internal/gitaly/server/auth/**"]
approvals = 2
approvers = ["security-*"]
```

Class names: `code`, `comment_only`, `docs`, `tests`, `generated`, `config`.
The configuration is rejected at load time when a rule has no name, a negative
`approvals`, an unknown class, or an empty or malformed glob. An absurd
`approvals` above 2147483647 is not rejected: the requirement then shows a
wrapped `required_approvals`, but it can never be satisfied (it fails closed).

Without any rule, the default applies:

```toml
[[graft.review.rule]]
name = "code"
classes = ["code", "config"]
approvals = 1
```

That is, code and configuration changes need one approval by someone other
than the author; comment-only, docs, tests and generated changes need none.

## Storage

Reviews live in the repository's git directory, under `graft/reviews/`:
`<id>.json` holds one review as protojson (without `requirements` and
`approved`, which are recomputed on read), `next_id` holds the next id, and
`.lock` is the file lock. Every mutation holds a per-directory in-process
mutex and an `flock` on `.lock`, and every write goes through a temporary
file, an fsync, a rename and an fsync of the directory. The id counter is
advanced before the review is written, so a crash leaves a gap, never a reused
id.

## SDK

On `*sdk.Repo`, with the proto messages as aliases (`sdk.Review`,
`sdk.ReviewFile`, `sdk.ReviewVerdict`, `sdk.ReviewComment`,
`sdk.ReviewRequirement`, `sdk.ReviewState`):

```go
func (r *Repo) CreateReview(ctx, CreateReviewOptions{TargetBranch, Head, Base, Title, Description}) (*Review, error)
func (r *Repo) GetReview(ctx, id string) (*Review, error)
func (r *Repo) ListReviews(ctx, ListReviewsOptions{TargetBranch, IncludeClosed, Limit}) ([]*Review, error)
func (r *Repo) UpdateReview(ctx, UpdateReviewOptions{ID, Head, ExpectedHead}) (*Review, error)
func (r *Repo) CommentReview(ctx, CommentReviewOptions{ID, Body, Path, Line}) (*ReviewComment, error)
func (r *Repo) ApproveReview(ctx, id, body string) (*Review, error)
func (r *Repo) ApproveReviewAt(ctx, VerdictOptions{ID, Head, Body}) (*Review, error)
func (r *Repo) RequestChanges(ctx, id, body string) (*Review, error)
func (r *Repo) RequestChangesAt(ctx, VerdictOptions{ID, Head, Body}) (*Review, error)
func (r *Repo) CloseReview(ctx, id string, landed bool) (*Review, error)
```

`TargetBranch` may be given with or without `refs/heads/`. `ApproveReview`
and `RequestChanges` read the review and give the verdict on its head at that
moment; prefer the `...At` forms with the head that was actually reviewed, so
that a head moved in between makes the call fail instead of approving unseen
code.

## CLI: `graft review`

```text
graft review create --branch B --title T [--base C] [--head H] [--description D]
graft review show ID [--json]
graft review list [--branch B] [--all] [--json]
graft review comment ID --body B [--path P [--line N]]
graft review approve ID [--head H] [--body B]
graft review request-changes ID [--head H] [--body B]
graft review update ID [--head H]
graft review close ID [--landed]
```

- Inside a workspace, the server and repository come from the workspace.
  Without a workspace, pass `-address`, `-storage` and `-repo`; then `create`
  and `update` need `--head`.
- `create` and `update` default the head to the workspace's pending commit
  (made by `graft commit` and not landed yet).
- `approve` and `request-changes` give the verdict on `--head` (a full ID, or a
  prefix of the review's current head as `show` prints it), or on the current
  head when `--head` is not set.
- `show` prints the header, the class of every file, the requirements, the
  verdicts (non-current ones marked `superseded`) and the comments. `list`
  prints one tab-separated line per review: id, state, branch, head, title.
  Text from other clients goes through the same escaping as other graft
  output, so a title with a tab or a newline cannot forge a row.
- `--json` prints each review as one protojson object per line; bytes fields
  (title, description, paths, bodies) are base64, as protojson encodes them.

## Deployment notes

- Agents may call every ReviewService RPC: the mutators only write the review
  store, never refs or objects.
- In Praefect, the review mutators are registered with transactions disabled.
  The review files are not part of what Praefect replicates (see the
  limitations).

## Limitations

- **Advisory only in phase 5a.** `approved` is not checked by `UserLand` or
  any other write; the land gate is phase 5b.
- **Path-only classes.** The path rules win over content. A source file under
  `test/`, `tests/`, `testdata/`, `doc/` or `docs/`, and a file named
  `*_generated.go`, `*.pb.go`, `*_pb2.py` or `*.min.js` without a generated
  marker, are classified tests, docs or generated by name. Under the default
  rules such files need no approval, even if they are compiled into the
  product. Approvals of such a file do not carry over an update (the carry-over
  rule only accepts documentation by name and comment-only changes), but the
  first approval is not required. Configure rules that cover these classes, or
  paths, where that matters.
- **Rebasing can hide a revert.** The files are always base..head. An author
  who updates the review to a head rebased onto a newer branch tip, and in the
  same head reverts a change that the newer tip brought, can make that revert
  invisible in the file list, because base..head then includes the tip's
  change and its revert. The land gate of phase 5b must compute the change
  against the real landing base.
- **The landed state is a label.** `CloseReview` with `landed` does not check
  that the head landed.
- **Not replicated, not backed up.** The review store is a directory in the
  git directory. Praefect replication and `gitaly-backup` copy refs and
  objects, not this directory, so reviews exist only on the node that handled
  the mutators; a Praefect read routed to a secondary may not see them.
  Reviews are intended for standalone Graft servers.
- **No authorization beyond authorship.** Anyone who can reach the repository
  can comment and give verdicts; the rules decide which verdicts count. Only
  the author may update or close a review. Without per-identity tokens every
  caller is `anonymous`, and then the author exclusion makes every approval by
  `anonymous` the author's own.
- **Lexers without an oracle.** The Go lexer is fuzzed against `go/scanner`
  (`FuzzGoMatchesGoScanner`). The Java, Rust and Python lexers are covered by
  table tests and by fuzzing for determinism and termination only; they have
  no reference implementation to compare with.
- **Scale.** `ListReviews` reads review files one by one, newest first, until
  the limit is reached; a repository with many closed reviews pays for reading
  them when filtering by branch or state. A review file that cannot be read or
  decoded makes the whole `ListReviews` call fail with `Internal`.
  Classification reads every changed blob (up to 1 MiB each) on create and on
  update.
