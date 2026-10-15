package superfan

import (
	"fmt"
	"strings"
	"time"
)

// Severities of findings, in increasing order.
const (
	SeverityNone   = "none"
	SeverityLow    = "low"
	SeverityMedium = "medium"
	SeverityHigh   = "high"
)

// Trailers the rules look for. They are written by the Graft commands and
// the resolver (see doc/graft/RESOLVE.md and VERIFY.md).
const (
	// TrailerAutoRevert is set on the revert commits of graft-verify.
	TrailerAutoRevert = "Graft-Auto-Revert"
	// TrailerResolvedBy is set on merge commits whose conflicts were resolved
	// by a resolver (possibly an AI strategy).
	TrailerResolvedBy = "Graft-Resolved-By"
)

// Rule names, as they appear in findings and in the JSON output.
const (
	RuleLargeChange        = "large-change"
	RuleRevert             = "revert"
	RuleAutoRevert         = "auto-revert"
	RuleAIResolved         = "ai-resolved"
	RuleProtectedPath      = "protected-path"
	RuleBurst              = "burst"
	RuleUnexpectedActor    = "unexpected-actor"
	RuleDetailsUnavailable = "details-unavailable"
	RuleBatchIncomplete    = "batch-incomplete"
)

// Finding is one deterministic observation about one changeset.
type Finding struct {
	Rule     string
	Severity string
	// Summary is one line of plain text, bounded and free of control
	// characters, because commit text is untrusted.
	Summary string
	Event   Event
	// Paths are the files involved, when the rule is about files. At most
	// maxFindingPaths are kept.
	Paths []string
	// Component is the component whose digest carries the finding. The engine
	// sets it.
	Component string
}

// severityRank orders severities; unknown values rank as none.
func severityRank(s string) int { return riskRank(s) }

// Analyzer evaluates the deterministic rules. It keeps the state of the burst
// rule and must see the events in order. It is not safe for concurrent use.
type Analyzer struct {
	cfg       RulesConfig
	operation string
	window    time.Duration
	// lands are the times of the most recent operations of the burst rule,
	// at most Burst.Lands+1 of them, the ones inside the window.
	lands []time.Time
	// lastBurst is the time of the last operation at which the burst threshold
	// was reached; zero before the first one.
	lastBurst time.Time
	// reportEvery is the period at which an ongoing burst is reported again:
	// the digest window (SetReportWindow), by default the burst window.
	reportEvery time.Duration
	// reported is the start of the report period of the last burst finding.
	reported time.Time
}

// NewAnalyzer builds an analyzer for the rules. The rules must be valid.
func NewAnalyzer(cfg RulesConfig) (*Analyzer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	a := &Analyzer{cfg: cfg, operation: defaultOperation}
	if b := cfg.Burst; b != nil {
		d, err := time.ParseDuration(b.Window)
		if err != nil {
			return nil, fmt.Errorf("superfan: burst window: %w", err)
		}
		a.window = d
		a.reportEvery = d
		if b.Operation != "" {
			a.operation = b.Operation
		}
	}
	return a, nil
}

// SetReportWindow sets the period at which an ongoing burst is reported again.
// The superfan passes its digest window, so that every digest window of a
// sustained burst carries one burst finding. Windows are aligned like the
// digest windows (multiples of d in event time). A non-positive d is ignored.
func (a *Analyzer) SetReportWindow(d time.Duration) {
	if d > 0 {
		a.reportEvery = d
	}
}

// Analyze returns the findings of one event. det is the commit details, or nil
// when they are unavailable; rules that need files are then skipped.
//
// The rules about who landed what judge every change of the event (see
// batchUnits): each member of a batched land is checked against the expected
// actors and counts toward the burst rule, with its own actor and commit in
// the finding. The rules that need files judge det, which describes one commit:
// the Superfan therefore analyzes the members of a batched land one by one,
// each with the details of its own commit.
func (a *Analyzer) Analyze(ev Event, det *Details) []Finding {
	out := a.analyzeFiles(ev, det)
	units, _ := batchUnits(ev)
	for _, u := range units {
		out = append(out, a.analyzeChange(u)...)
	}
	return out
}

// analyzeFiles applies the rules that need the commit details.
func (a *Analyzer) analyzeFiles(ev Event, det *Details) []Finding {
	if det == nil {
		return nil
	}
	var out []Finding
	add := func(rule, sev, summary string, paths []string) {
		out = append(out, Finding{Rule: rule, Severity: sev, Summary: summary, Event: ev, Paths: paths})
	}

	if lc := a.cfg.LargeChange; lc != nil {
		files, lines := det.ChangedFiles(), det.Lines()
		if (lc.MaxFiles > 0 && files > lc.MaxFiles) || (lc.MaxLines > 0 && lines > lc.MaxLines) {
			add(RuleLargeChange, SeverityMedium,
				fmt.Sprintf("%d files and %d changed lines (limits %d files, %d lines)",
					files, lines, lc.MaxFiles, lc.MaxLines), nil)
		}
	}
	if auto, ok := det.Trailer(TrailerAutoRevert); ok {
		add(RuleAutoRevert, SeverityMedium, labelled("automatic revert of ", auto), nil)
	} else if isRevert(det) {
		add(RuleRevert, SeverityLow, labelled("revert: ", det.Subject), nil)
	}
	if by, ok := det.Trailer(TrailerResolvedBy); ok {
		add(RuleAIResolved, SeverityMedium, labelled("conflicts resolved by ", by), nil)
	}
	if hits, total := a.protectedHits(det); total > 0 {
		add(RuleProtectedPath, SeverityHigh, protectedSummary(hits, total), hits)
	}
	return out
}

// analyzeChange applies the rules about who landed a change and when: the
// expected actors and the burst. ev is one change (see batchUnits).
func (a *Analyzer) analyzeChange(ev Event) []Finding {
	var out []Finding
	add := func(rule, sev, summary string) {
		out = append(out, Finding{Rule: rule, Severity: sev, Summary: summary, Event: ev})
	}

	if ev.part == unlistedPart {
		// The tip of a batch whose member list is incomplete carries BatchActor,
		// the identity of the combined update. The actors and commits of the
		// unlisted changes are unknown, which is reported instead of judging
		// BatchActor as an actor.
		add(RuleBatchIncomplete, SeverityMedium,
			"batched land does not list all of its changes: the changes after the listed members were not inspected one by one and their actors are unknown")
	} else if len(a.cfg.ExpectedActors) > 0 && !matchAny(a.cfg.ExpectedActors, ev.Actor) {
		add(RuleUnexpectedActor, SeverityHigh,
			fmt.Sprintf("actor %q is not expected", oneLine(ev.Actor, maxLabelText)))
	}

	if a.cfg.Burst != nil && a.countsForBurst(ev.Operation) {
		if fire, ongoing := a.burst(ev.RecordedAt); fire {
			summary := fmt.Sprintf("%d %s operations within %s", a.cfg.Burst.Lands, oneLine(a.operation, maxLabelText), a.window)
			if ongoing {
				summary += " (the burst continues)"
			}
			add(RuleBurst, SeverityHigh, summary)
		}
	}
	return out
}

// burst records an operation at t and reports whether the burst rule fires,
// and whether the burst was already going on.
//
// A burst starts at the operation that brings the count of operations within
// the window to Burst.Lands, and goes on as long as the count reaches
// Burst.Lands again within one window of the last time it did. That gap is the
// hysteresis: a steady rate right at the threshold, where the count goes up and
// down by one, is one burst and not a new burst at every operation. The rule
// fires when a burst starts, and then at most once per report period (the
// digest window) while it goes on, so that every digest window of a sustained
// burst says so and none says it more than once.
func (a *Analyzer) burst(t time.Time) (fire, ongoing bool) {
	if a.recordLand(t) < a.cfg.Burst.Lands {
		return false, false
	}
	ongoing = !a.lastBurst.IsZero() && t.Sub(a.lastBurst) <= a.window
	if t.After(a.lastBurst) {
		a.lastBurst = t
	}
	period := t.Truncate(a.reportEvery)
	if ongoing && !period.After(a.reported) {
		return false, true
	}
	a.reported = period
	return true, ongoing
}

// countsForBurst reports whether a change of the operation op counts toward the
// burst rule. The members of a batched land count as lands: batching folds
// concurrent lands into land_batch events, so without them the rule would not
// see the very bursts it is meant to report.
func (a *Analyzer) countsForBurst(op string) bool {
	return op == a.operation || (a.operation == OperationLand && op == OperationLandBatch)
}

// recordLand adds an operation at t and returns how many operations lie in the
// window ending at t, capped at Burst.Lands+1, which is enough to tell whether
// the count reached Burst.Lands.
//
// Only the newest Burst.Lands+1 operations are kept. If the window holds k of
// them, the newest k are all in it, so the count is exact up to the cap and the
// state stays bounded whatever the rate of events.
func (a *Analyzer) recordLand(t time.Time) int {
	cutoff := t.Add(-a.window)
	kept := a.lands[:0]
	for _, at := range a.lands {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	a.lands = append(kept, t)
	if limit := a.cfg.Burst.Lands + 1; len(a.lands) > limit {
		a.lands = a.lands[len(a.lands)-limit:]
	}
	return len(a.lands)
}

// protectedHits returns the first changed paths that match a protected glob,
// at most maxFindingPaths of them, and the total number of such paths. Paths
// are matched as the commit spells them (MatchFile) and returned as bounded
// plain text (oneLine), since they are untrusted.
func (a *Analyzer) protectedHits(det *Details) ([]string, int) {
	var hits []string
	total := 0
	for _, f := range det.Files {
		if !MatchFile(a.cfg.ProtectedPaths, f.Path) {
			continue
		}
		total++
		if len(hits) < maxFindingPaths {
			hits = append(hits, oneLine(f.Path, maxPathText))
		}
	}
	return hits, total
}

// labelled returns prefix followed by the text s, as one bounded line. s is cut
// before the join, so that a huge commit subject costs nothing.
func labelled(prefix, s string) string {
	return oneLine(prefix+oneLine(s, maxSummaryText), maxSummaryText)
}

// protectedSummary describes the protected paths of a changeset in one bounded
// line. hits are the first paths, total counts all of them.
func protectedSummary(hits []string, total int) string {
	const more = " (and 999999 more)" // room for the count of the paths not listed
	var b strings.Builder
	b.WriteString("touches protected paths: ")
	listed := 0
	for _, p := range hits {
		item := oneLine(p, maxPathText)
		sep := ""
		if listed > 0 {
			sep = ", "
		}
		if b.Len()+len(sep)+len(item)+len(more) > maxSummaryText {
			break
		}
		b.WriteString(sep)
		b.WriteString(item)
		listed++
	}
	if total > listed {
		fmt.Fprintf(&b, " (and %d more)", total-listed)
	}
	return b.String()
}

// isRevert reports whether the commit is a revert made by git revert or by
// Graft's revert of a subject ("Revert \"...\"").
func isRevert(det *Details) bool {
	if strings.HasPrefix(det.Subject, `Revert "`) {
		return true
	}
	for _, line := range strings.Split(det.Message, "\n") {
		if strings.HasPrefix(line, "This reverts commit ") {
			return true
		}
	}
	return false
}
