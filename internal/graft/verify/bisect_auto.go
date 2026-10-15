package verify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	"github.com/feder-cr/open_scion/internal/graft/workspace"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// Committer of the automatic reverts when the configuration names none. The
// verifier records itself, so that its own reverts are recognized later.
const (
	DefaultRevertCommitterName  = "graft-verify"
	DefaultRevertCommitterEmail = "graft-verify@localhost"
)

// NewBisector returns the bisector that graft-verify runs for cfg. It searches
// the first-parent range of a failing tip over conn, the connection of the
// verified server, in repo. When cfg.Revert.Enabled is set, the culprit is
// reverted with the reverts and the cap of cfg (dry run included), and the
// outcome is notified to the webhook of cfg or to stdout. Otherwise the culprit
// is only reported. backend resolves the branch tips.
func NewBisector(cfg Config, conn *grpc.ClientConn, repo *gitalypb.Repository, backend workspace.Backend) (*AutoBisector, error) {
	if conn == nil || repo == nil || backend == nil {
		return nil, errors.New("verify: bisector needs a connection, a repository and a backend")
	}

	var notifier Notifier = &StdoutNotifier{}
	if cfg.Revert.WebhookURL != "" {
		notifier = &WebhookNotifier{URL: cfg.Revert.WebhookURL}
	}

	committer := Signature{Name: DefaultRevertCommitterName, Email: DefaultRevertCommitterEmail}
	operations := gitalypb.NewOperationServiceClient(conn)
	lister := &CommitLister{Commits: gitalypb.NewCommitServiceClient(conn), Repository: repo}
	b := &AutoBisector{Commits: lister, Notifier: notifier}
	if !cfg.Revert.Enabled {
		return b, nil
	}

	b.Reverts = &AutoReverter{
		Reverter: &OperationReverter{
			Operations: operations,
			Repository: repo,
			TipOf:      BranchTip(backend),
		},
		History:  lister,
		Notifier: notifier,
		Policy: RevertPolicy{
			DryRun:     cfg.Revert.DryRun,
			MaxPerHour: cfg.Revert.MaxPerHour,
			Committer:  committer,
		},
	}
	// A dry run writes nothing, scratch branches included. It also never
	// reverts, so its ranges hold no automatic revert of its own.
	if !cfg.Revert.DryRun {
		b.Scratch = &OperationScratch{Operations: operations, Repository: repo, Committer: committer}
	}
	return b, nil
}

// AutoBisector implements Bisector: it finds the culprit of a failing tip and
// either reverts it through an AutoReverter or, when Reverts is nil, only
// reports it through Notifier.
type AutoBisector struct {
	// Commits lists the first-parent range that is searched.
	Commits CommitSource
	// Reverts reverts the culprit. When nil, the culprit is reported only.
	Reverts *AutoReverter
	// Notifier reports the culprit when Reverts is nil. It may be nil.
	Notifier Notifier
	// Scratch builds the commits that are tested with the culprits undone that
	// an automatic revert of the searched range already reverted. When nil, such
	// a range cannot be searched and Bisect fails. See CulpritFinder.
	Scratch ScratchReverter
	// Logger records every decision. It defaults to slog.Default.
	Logger *slog.Logger
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time
}

var _ Bisector = (*AutoBisector)(nil)

// Bisect implements Bisector. A failure to search is returned, and the cycle
// records it. Every decision that the search reaches is logged and notified: a
// flaky tip, a tip without culprit, a culprit that was reverted, refused or
// only reported, and a failing tip with no green baseline yet (the first
// failure of a new branch), which is not bisected. A notification that fails
// is returned too, so that the decision still reaches the outcome of the cycle.
func (b *AutoBisector) Bisect(ctx context.Context, f Failure) error {
	if f.Good == "" {
		out := RevertOutcome{
			At:     b.now(),
			Status: StatusNoBaseline,
			Branch: f.Branch,
			Tip:    f.Bad,
			Reason: "no green commit is known yet: the failing tip is not bisected",
		}
		if err := b.notify(ctx, out); err != nil {
			out.NotifyError = err.Error()
		}
		b.log(out)
		return errors.Join(ErrNoBaseline, notifyError(out))
	}
	if f.Tester == nil {
		return errors.New("verify: failure has no tester")
	}
	if b.Commits == nil {
		return errors.New("verify: bisector has no commit source")
	}

	finder := &CulpritFinder{
		Tester:   verdictTester{tester: f.Tester},
		Commits:  b.Commits,
		RevertOf: autoRevertOf(b.committer()),
		Scratch:  b.Scratch,
	}
	res, err := finder.Find(ctx, f.Good, f.Bad)
	if err != nil {
		return fmt.Errorf("bisect %s..%s: %w", f.Good, f.Bad, err)
	}

	if b.Reverts != nil {
		out, err := b.Reverts.Apply(ctx, f.Branch, res)
		if out.Status == "" {
			// Apply failed before deciding anything; nothing was notified.
			return err
		}
		b.log(out)
		return errors.Join(err, notifyError(out))
	}
	return b.report(ctx, f.Branch, res)
}

// committer returns the committer of the automatic reverts.
func (b *AutoBisector) committer() Signature {
	if b.Reverts != nil {
		return b.Reverts.Policy.Committer
	}
	return Signature{Name: DefaultRevertCommitterName, Email: DefaultRevertCommitterEmail}
}

// autoRevertOf returns the function that recognizes the automatic reverts of
// committer and returns the culprit they name. As for the guards of the
// reverts, only the commits of the verifier's committer count, and without a
// committer every commit does. A commit whose message was truncated is not
// taken as a revert, since its trailers are unknown.
func autoRevertOf(committer Signature) func(CommitInfo) (string, bool) {
	return func(c CommitInfo) (string, bool) {
		if committer.Email != "" && c.CommitterEmail != committer.Email {
			return "", false
		}
		if c.Truncated {
			return "", false
		}
		target := autoRevertTarget(c.Message)
		return target, target != ""
	}
}

// notifyError returns the failure of the notification of out, or nil.
func notifyError(out RevertOutcome) error {
	if out.NotifyError == "" {
		return nil
	}
	return fmt.Errorf("notify %s outcome (culprit %q, revert %q): %s", out.Status, out.Culprit, out.RevertCommit, out.NotifyError)
}

// log records out.
func (b *AutoBisector) log(out RevertOutcome) {
	logger := b.Logger
	if logger == nil {
		logger = slog.Default()
	}
	level := slog.LevelInfo
	if out.NotifyError != "" {
		level = slog.LevelError
	}
	logger.Log(context.Background(), level, "bisect decision",
		"status", string(out.Status),
		"tip", out.Tip,
		"culprit", out.Culprit,
		"subject", out.Subject,
		"revert_commit", out.RevertCommit,
		"reason", out.Reason,
		"dry_run", out.DryRun,
		"notify_error", out.NotifyError,
	)
}

// report notifies the outcome of a search when reverts are disabled. Nothing
// is changed on the branch. A failing notifier is returned, so that the cycle
// records it.
func (b *AutoBisector) report(ctx context.Context, branch string, res *BisectResult) error {
	out := RevertOutcome{At: b.now(), Branch: branch, Tip: res.Tip}
	switch {
	case res.Flaky:
		out.Status, out.Reason = StatusFlaky, "tip passed on the re-run"
	case res.Culprit == nil:
		out.Status, out.Reason = StatusNoCulprit, "no failing commit found"
	default:
		out.Status = StatusCulprit
		out.Culprit, out.Subject = res.Culprit.ID, res.Culprit.Subject
		out.Reason = "reverts are disabled: the culprit is reported only"
	}
	if err := b.notify(ctx, out); err != nil {
		out.NotifyError = err.Error()
	}
	b.log(out)
	return notifyError(out)
}

// notify sends out to the notifier, when there is one. A failing notifier is
// returned.
func (b *AutoBisector) notify(ctx context.Context, out RevertOutcome) error {
	if b.Notifier == nil {
		return nil
	}
	if err := b.Notifier.Notify(ctx, out); err != nil {
		return fmt.Errorf("notify outcome: %w", err)
	}
	return nil
}

func (b *AutoBisector) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}
