package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Sentinel errors returned by a Reverter.
var (
	// ErrBranchMoved means the branch no longer points to ExpectedOld. The
	// caller re-reads the tip and reverts onto it.
	ErrBranchMoved = errors.New("verify: branch moved before the revert was applied")
	// ErrRevertConflict means the revert does not apply cleanly onto the tip.
	ErrRevertConflict = errors.New("verify: revert conflicts with the branch tip")
	// ErrAlreadyReverted means the culprit's changes are already undone on the
	// branch, so the revert would be empty.
	ErrAlreadyReverted = errors.New("verify: culprit is already reverted on the branch")
)

const (
	// AutoRevertTrailer marks the commits created by graft-verify. Its value is
	// the culprit commit ID.
	AutoRevertTrailer = "Graft-Auto-Revert"

	// DefaultMaxAttempts bounds the revert retries when the branch keeps moving.
	DefaultMaxAttempts = 5

	// revertWindow is the sliding window of the revert cap.
	revertWindow = time.Hour

	// maxRevertSubjectBytes bounds the culprit subject quoted in an automatic
	// revert. The trailer follows the subject, and the server keeps only a bounded
	// part of a message, so a long subject would lose the trailer.
	maxRevertSubjectBytes = 200
)

// Reverter applies reverts on the server.
type Reverter interface {
	// Tip returns the commit the branch currently points to.
	Tip(ctx context.Context, branch string) (string, error)
	// Revert reverts req.Commit onto req.Branch, provided the branch still
	// points to req.ExpectedOld, and returns the new commit ID. It returns
	// ErrBranchMoved, ErrRevertConflict or ErrAlreadyReverted for the known
	// failures.
	Revert(ctx context.Context, req RevertRequest) (string, error)
}

// RevertHistory reads the commits of a branch. The revert cap is counted from
// it rather than kept in memory, so that the cap holds across processes (a CI
// job runs graft-verify -once for every land) and across restarts.
type RevertHistory interface {
	// CommitsSince returns the first-parent commits of branch committed at or
	// after since, oldest first, with their trailers in Message.
	CommitsSince(ctx context.Context, branch string, since time.Time) ([]CommitInfo, error)
}

// RevertRequest describes one revert.
type RevertRequest struct {
	Branch      string
	Commit      string
	ExpectedOld string
	Message     string
	Committer   Signature
	// At is the time the verifier decided on the revert. The server stamps the
	// commit with its own clock, so At is only what the decision was made at.
	At time.Time
}

// Signature identifies the committer of automatic reverts.
type Signature struct {
	Name  string
	Email string
}

// RevertPolicy configures automatic reverts.
type RevertPolicy struct {
	// DryRun reports what would be reverted without touching the branch.
	DryRun bool
	// MaxPerHour caps the number of automatic reverts in any sliding hour,
	// counted from the branch history. Zero refuses every revert: it is the
	// value of revert.max_per_hour when the key is omitted, and the operator's
	// configuration says that there are no reverts then.
	MaxPerHour int
	// MaxAttempts bounds the retries when the branch moves. Values below one
	// select DefaultMaxAttempts.
	MaxAttempts int
	// Committer is recorded as the committer of the revert commits. Culprits
	// committed by this committer that carry the trailer are never reverted, and
	// only the commits of this committer count toward the cap.
	//
	// Residual risk: the server does not bind the committer of a commit to the
	// authenticated caller, so an agent can commit with this email and the
	// trailer. Such a commit is then immune to auto-revert (and is refused, with a
	// notification), and its trailers use up the cap. Both are operator-visible
	// through the notifications. See doc/graft/VERIFY.md.
	Committer Signature
}

// Status is the outcome of AutoReverter.Apply or AutoBisector.Bisect.
type Status string

// Outcomes reported by Apply and Bisect.
const (
	StatusReverted        Status = "reverted"
	StatusDryRun          Status = "dry-run"
	StatusAlreadyReverted Status = "already-reverted"
	StatusRefused         Status = "refused"
	StatusFlaky           Status = "flaky"
	StatusNoCulprit       Status = "no-culprit"
	// StatusNoBaseline reports a failing tip for which no green commit is known
	// yet, so that nothing could be bisected.
	StatusNoBaseline Status = "no-baseline"
	// StatusCulprit reports a culprit that was found but not reverted, because
	// reverts are disabled.
	StatusCulprit Status = "culprit-found"
)

// RevertOutcome describes what Apply did. It is also the payload of the
// notifications.
type RevertOutcome struct {
	At           time.Time `json:"at"`
	Status       Status    `json:"status"`
	Branch       string    `json:"branch"`
	Tip          string    `json:"tip"`
	Culprit      string    `json:"culprit,omitempty"`
	Subject      string    `json:"subject,omitempty"`
	RevertCommit string    `json:"revert_commit,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	DryRun       bool      `json:"dry_run"`
	// NotifyError is set when the notification of the outcome failed. The
	// revert itself is not affected.
	NotifyError string `json:"-"`
}

// AutoReverter reverts a culprit found by CulpritFinder and notifies the
// outcome. It is safe for concurrent use: the cap is counted and the revert is
// sent under one lock, so concurrent calls cannot exceed the cap.
type AutoReverter struct {
	Reverter Reverter
	// History counts the automatic reverts of the sliding hour. It is required:
	// without it the cap cannot be enforced, so Apply reverts nothing.
	History  RevertHistory
	Notifier Notifier
	Policy   RevertPolicy
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time

	mu sync.Mutex
}

// Apply reverts the culprit of res onto branch, unless a guard refuses it.
// Every decision, including refusals and dry runs, is notified. The returned
// error is set only when the revert failed for a reason other than the known
// outcomes; such a failure is not notified.
func (a *AutoReverter) Apply(ctx context.Context, branch string, res *BisectResult) (RevertOutcome, error) {
	if res == nil {
		return RevertOutcome{}, errors.New("verify: nil bisect result")
	}
	out := RevertOutcome{At: a.now(), Branch: branch, Tip: res.Tip, DryRun: a.Policy.DryRun}

	out, err := a.apply(ctx, branch, res, out)
	if out.Status == "" {
		return out, err
	}
	if a.Notifier != nil {
		if nerr := a.Notifier.Notify(ctx, out); nerr != nil {
			out.NotifyError = nerr.Error()
		}
	}
	return out, err
}

func (a *AutoReverter) apply(ctx context.Context, branch string, res *BisectResult, out RevertOutcome) (RevertOutcome, error) {
	switch {
	case res.Flaky:
		out.Status, out.Reason = StatusFlaky, "tip passed on the re-run"
		return out, nil
	case res.Culprit == nil:
		out.Status, out.Reason = StatusNoCulprit, "no failing commit found"
		return out, nil
	}

	c := *res.Culprit
	out.Culprit, out.Subject = c.ID, c.Subject

	if reason := refuseReason(c, a.Policy.Committer); reason != "" {
		out.Status, out.Reason = StatusRefused, reason
		return out, nil
	}

	if a.History == nil || a.Reverter == nil {
		return out, errors.New("verify: auto-reverter needs a reverter and a revert history")
	}

	// The count, the decision and the revert happen under one lock. The revert
	// is in the history once it returns, so the next call counts it; no slot
	// needs to be reserved or given back.
	a.mu.Lock()
	defer a.mu.Unlock()

	count, err := a.countReverts(ctx, branch, out.At)
	if err != nil {
		out.Status, out.Reason = StatusRefused, "cannot count the reverts of the last hour: "+err.Error()
		return out, nil
	}
	if count >= a.Policy.MaxPerHour {
		out.Status, out.Reason = StatusRefused, a.capReason(count)
		return out, nil
	}

	if a.Policy.DryRun {
		out.Status, out.Reason = StatusDryRun, "dry run: the culprit would be reverted"
		return out, nil
	}

	req := RevertRequest{
		Branch:    branch,
		Commit:    c.ID,
		Message:   revertMessage(c),
		Committer: a.Policy.Committer,
		At:        out.At,
	}

	for attempt := 0; attempt < a.maxAttempts(); attempt++ {
		tip, err := a.Reverter.Tip(ctx, branch)
		if err != nil {
			return out, fmt.Errorf("read tip of %s: %w", branch, err)
		}
		if tip == "" {
			return out, fmt.Errorf("read tip of %s: empty object ID", branch)
		}
		req.ExpectedOld = tip
		out.Tip = tip

		id, err := a.Reverter.Revert(ctx, req)
		switch {
		case err == nil:
			if id == "" {
				return out, fmt.Errorf("revert %s on %s: no commit ID returned", c.ID, branch)
			}
			out.Status, out.RevertCommit = StatusReverted, id
			return out, nil
		case errors.Is(err, ErrBranchMoved):
			continue
		case errors.Is(err, ErrAlreadyReverted):
			// The automatic reverts of the range are taken into account by the
			// search, so this culprit was undone by other means, for example a
			// revert by hand, and the tip still fails: an operator must look.
			out.Status, out.Reason = StatusAlreadyReverted,
				"the culprit is already undone on the branch and the tip still fails: the branch needs a fix by hand"
			return out, nil
		case errors.Is(err, ErrRevertConflict):
			out.Status, out.Reason = StatusRefused, "the revert conflicts with the current tip"
			return out, nil
		default:
			return out, fmt.Errorf("revert %s on %s: %w", c.ID, branch, err)
		}
	}

	out.Status = StatusRefused
	out.Reason = fmt.Sprintf("the branch kept moving: gave up after %d attempts", a.maxAttempts())
	return out, nil
}

// countReverts returns the number of commits of the sliding hour ending at now
// that are automatic reverts. Only the commits of the verifier's committer are
// counted, so that a commit of another author cannot use up the cap by carrying
// the trailer. A commit whose date is not reported is counted too, and so is a
// commit whose message was truncated by the server, since its trailers are
// unknown. The caller holds mu.
func (a *AutoReverter) countReverts(ctx context.Context, branch string, now time.Time) (int, error) {
	since := now.Add(-revertWindow)
	commits, err := a.History.CommitsSince(ctx, branch, since)
	if err != nil {
		return 0, fmt.Errorf("list commits of %s: %w", branch, err)
	}
	n := 0
	for _, c := range commits {
		if !c.CommittedAt.IsZero() && c.CommittedAt.Before(since) {
			continue
		}
		if !a.isVerifierCommit(c) {
			continue
		}
		if c.Truncated || hasAutoRevertTrailer(c.Message) {
			n++
		}
	}
	return n, nil
}

// isVerifierCommit reports whether c is committed by the verifier. Without a
// configured committer every commit is taken as the verifier's.
func (a *AutoReverter) isVerifierCommit(c CommitInfo) bool {
	return a.Policy.Committer.Email == "" || c.CommitterEmail == a.Policy.Committer.Email
}

func (a *AutoReverter) capReason(count int) string {
	if a.Policy.MaxPerHour <= 0 {
		return "reverts are disabled: the revert cap is zero"
	}
	return fmt.Sprintf("revert cap reached: %d reverts in the last hour, the limit is %d", count, a.Policy.MaxPerHour)
}

func (a *AutoReverter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *AutoReverter) maxAttempts() int {
	if a.Policy.MaxAttempts < 1 {
		return DefaultMaxAttempts
	}
	return a.Policy.MaxAttempts
}

// refuseReason returns why the culprit must not be reverted automatically, or
// an empty string. An auto-revert is never reverted again, because reverting it
// would reintroduce the change it undid. Only the commits of the verifier's own
// committer count: an agent cannot make its commit immune by writing the trailer
// itself. Any other revert, hand-written or not, is a normal culprit. A culprit
// of the verifier's committer whose message was truncated by the server is
// refused too, since its trailers are unknown.
func refuseReason(c CommitInfo, committer Signature) string {
	if committer.Email != "" && c.CommitterEmail != committer.Email {
		return ""
	}
	switch {
	case hasAutoRevertTrailer(c.Message):
		return "the culprit is itself an auto-revert"
	case c.Truncated:
		return "the culprit's message is too long to tell whether it is an auto-revert"
	}
	return ""
}

// shortenSubject bounds s to maxRevertSubjectBytes, cutting at a character
// boundary and marking the cut with an ellipsis.
func shortenSubject(s string) string {
	if len(s) <= maxRevertSubjectBytes {
		return s
	}
	cut := maxRevertSubjectBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// revertMessage builds the message of an automatic revert:
//
//	Revert "<subject>"
//
//	This reverts commit <id>, which failed the verification test command.
//
//	Graft-Auto-Revert: <id>
func revertMessage(c CommitInfo) string {
	var b strings.Builder
	b.WriteString(`Revert "`)
	b.WriteString(shortenSubject(c.Subject))
	b.WriteString("\"\n\n")
	b.WriteString("This reverts commit ")
	b.WriteString(c.ID)
	b.WriteString(", which failed the verification test command.\n\n")
	b.WriteString(AutoRevertTrailer)
	b.WriteString(": ")
	b.WriteString(c.ID)
	b.WriteString("\n")
	return b.String()
}

// hasAutoRevertTrailer reports whether the trailer block of msg, its last
// paragraph, holds the auto-revert trailer. A message with no blank line is
// taken as a single block. Trailers in the middle of a message do not count.
func hasAutoRevertTrailer(msg string) bool {
	return hasTrailer(trailerBlock(msg), AutoRevertTrailer)
}

// autoRevertTarget returns the culprit that the auto-revert trailer of msg
// names, or "" when the trailer block of msg has no such trailer. The first
// trailer counts.
func autoRevertTarget(msg string) string {
	prefix := AutoRevertTrailer + ":"
	for _, line := range strings.Split(trailerBlock(msg), "\n") {
		if value, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// trailerBlock returns the last paragraph of msg, which holds its trailers. A
// message with no blank line is a single block.
func trailerBlock(msg string) string {
	msg = strings.TrimRight(msg, " \t\r\n")
	if i := strings.LastIndex(msg, "\n\n"); i >= 0 {
		return msg[i+2:]
	}
	return msg
}

// hasTrailer reports whether a line of msg starts with key + ":".
func hasTrailer(msg, key string) bool {
	prefix := key + ":"
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}
