// Package superfan implements the Graft monitoring agent ("superfan"). It
// follows the changeset log, applies deterministic rules to every changeset,
// and delivers one digest per component and time window to notifiers.
//
// The package never talks to the Graft server directly and does not import
// the Gitaly module. Its inputs are two documented formats:
//
//   - Events: one JSON object per line, as printed by `graft watch --json`
//     (or `graft log --json`). Fields: sequence (uint64, dense from 1),
//     reference (for example refs/heads/main), old_oid, new_oid (full object
//     IDs; old_oid may be the zero ID), operation (for example "land"),
//     actor, recorded_at (RFC 3339), rewritten (bool), and members for a
//     batched land (operation "land_batch", actor "graft-batch"): one object
//     per landed change with original_commit, commit, actor, rewritten and
//     resolved_paths. Unknown fields are ignored. The events are read from
//     standard input, a file, or the stdout of a command (see EventSource).
//   - Batched lands: the rules judge each member as a change of its own, as
//     they judged each land before batching (see batchUnits). Every member is
//     checked against the expected actors (graft-batch, the identity of the
//     combined update, is not), counts toward the burst rule (which counts
//     land_batch members as lands), and has the details of its own commit
//     fetched, so that the file rules see every member's change. A batch whose
//     member list was cut by the server (its last member is not new_oid) or
//     is empty gets a medium batch-incomplete finding, its tip is inspected,
//     and it is filed like a changeset whose details are unknown.
//   - Details: one JSON object per commit, printed by `graft show --json
//     REV`: oid, parents, author, subject, message (the whole message),
//     trailers (map from trailer key to values), files ([{path, status,
//     additions, deletions}]), and peeled_from when REV was an annotated tag
//     peeled to the commit oid (the details then match an event whose new_oid
//     is the tag). A rename is two files: the new path with status "renamed"
//     and the old path with status "renamed-from" (no line counts), so that the
//     rules see both sides. Paths are kept as the commit spells them, whatever
//     characters git allowed in them; they are matched with MatchFile and
//     sanitized only for display. Details are fetched only when a rule or a
//     component needs them (DetailSource, CommandDetails). Details larger than
//     the limit (ErrDetailsTooLarge) or unreadable are findings: a large-change
//     finding and a medium details-unavailable finding, since the other rules
//     cannot run without them. Such a changeset may touch any component, so it
//     is filed under UnassignedComponent and under every component that a route
//     names.
//
// Rules (see Analyzer): large-change (files or changed lines above a limit; a
// rename counts as one file, see Details.ChangedFiles),
// revert (git revert or a subject starting with Revert "), auto-revert (the
// Graft-Auto-Revert trailer), ai-resolved (the Graft-Resolved-By trailer),
// protected-path (a changed path matches a protected glob), burst (N
// operations of one kind within a window; reported when the burst starts and
// once more in each later digest window while the threshold keeps being
// reached within one burst window), unexpected-actor (the actor
// matches no expected glob), batch-incomplete (a batched land does not list
// all of its changes). Each finding has a severity (low, medium, high).
//
// Components map path globs to names. A changeset is filed under every
// component one of whose paths it touches, or under UnassignedComponent. The
// digest of a component for a window lists its changesets (sequence, reference,
// actor, operation, subject, the changed paths of the component), its findings,
// the number of its changesets and the highest severity. Digests with no finding
// are not sent unless digest.include_quiet is set. Routes send the digests of a
// component (or of "*") to named notifiers.
//
// The optional "references" list of globs restricts the analysis to the
// references it names. Other events are skipped, neither analyzed nor fetched.
// Deletions (a zero new object ID) have no commit, so they are not fetched.
//
// Windows are aligned on multiples of the window length in event time. A
// window closes when an event falls after it, or, when TickInterval is set,
// when the wall clock passes it. Closing a window delivers its digests and
// then saves the cursor (CursorStore) with the last handled sequence, so a
// restart resumes without repeating a delivered digest. Delivery is
// at-least-once: when a notifier fails, the cursor is held at the sequence
// before that window and stays there for the rest of the run, so a restart
// delivers the window again (digests of later windows may repeat too). Events
// of an open window that were not delivered are read again after a restart.
// Events skipped by "references" open no window, but the progress they make is
// saved too, at the end of the run and at every tick, when no window is open.
//
// A window with changesets but no finding is not delivered unless
// digest.include_quiet is set. This departs from "a digest per component per
// window" on purpose: a quiet component would otherwise send one message per
// window to every notifier. Set include_quiet for the literal behavior.
//
// The optional Summarizer (summarize*.go, registered with RegisterSummarizer)
// adds a text summary and may raise the risk of a digest. Its failures are
// logged and the digest is sent without it. The default summarizer "none"
// makes no summaries.
//
// The configuration is a JSON document (ParseConfig) that rejects unknown
// fields, so that a misspelled key cannot silently disable a rule. The
// webhook payload is the JSON encoding of notify.Message.
package superfan
