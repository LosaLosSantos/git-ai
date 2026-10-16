package superfan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/notify"
)

const (
	// maxDigestItems bounds the findings kept in one digest. The findings past
	// it are counted in Digest.FindingsOmitted.
	maxDigestItems = 200
	// maxSummaryBytes bounds the summary text of one digest.
	maxSummaryBytes = 4000
	// maxRecordedErrors bounds the delivery errors kept for Run's result.
	maxRecordedErrors = 100
	// finishTimeout bounds the deliveries made after the run ends.
	finishTimeout = 30 * time.Second
)

// Digest is what one component received in one window. It is what the
// summarizer reads: the changesets of the component (Entries), the findings
// of its rules and the highest severity. Both lists are bounded; the counts of
// what did not fit are kept.
type Digest struct {
	Component string
	Start     time.Time
	End       time.Time
	// Events is the number of changesets of the component in the window. Each
	// change of a batched land counts as one (see batchUnits).
	Events int
	// Entries describe the changesets of the component, in order, at most
	// maxDigestEntries of them. A batched land has one entry per change.
	Entries        []DigestEntry
	EntriesOmitted int
	// Findings are the findings of the rules, at most maxDigestItems of them.
	Findings        []Finding
	FindingsOmitted int
	// Risk is the highest severity of the findings.
	Risk string
}

// DigestEntry describes one changeset of a component in a window: what
// happened, as far as the events and the commit details tell.
type DigestEntry struct {
	Sequence   uint64
	Reference  string
	Operation  string
	Actor      string
	OID        string
	RecordedAt time.Time
	// Subject is the commit subject. It is empty when the details are unknown.
	Subject string
	// Files are the changed paths of the changeset that belong to the
	// component (every changed path for UnassignedComponent), at most
	// maxEntryFiles of them.
	Files []string
	// FilesOmitted counts the changed paths not listed in Files.
	FilesOmitted int
}

// Options configure a Superfan. Only Config and Source are required.
type Options struct {
	Config *Config
	Source EventSource
	// Details fetches commit details. Without it, rules that need files are
	// skipped and every changeset is filed under UnassignedComponent.
	Details DetailSource
	// Notifiers replaces the notifiers built from the configuration.
	Notifiers map[string]notify.Notifier
	// Stdout is the writer of "stdout" notifiers; default os.Stdout.
	Stdout io.Writer
	// Summarizer replaces the summarizer built from the configuration.
	Summarizer Summarizer
	// Cursor persists progress. Nil keeps it in memory only.
	Cursor *CursorStore
	// After skips events with a sequence at or below it. Pass the loaded cursor.
	After uint64
	// Clock is the wall clock used by TickInterval; default time.Now.
	Clock func() time.Time
	// TickInterval closes a window by the wall clock once it has elapsed, so
	// that an idle stream still delivers digests. Zero or negative disables it,
	// which is what replays want.
	TickInterval time.Duration
	// Logger receives diagnostics; default slog.Default().
	Logger *slog.Logger
}

// Superfan consumes events, applies the rules and delivers one digest per
// component and window. It is not safe for concurrent use: Run once.
type Superfan struct {
	cfg        *Config
	src        EventSource
	details    DetailSource
	notifiers  map[string]notify.Notifier
	summarizer Summarizer
	cursor     *CursorStore
	clock      func() time.Time
	tick       time.Duration
	log        *slog.Logger
	analyzer   *Analyzer
	window     time.Duration
	components []ComponentConfig
	// componentPaths maps a component name to its path globs.
	componentPaths map[string][]string
	references     []string
	routes         []RouteConfig

	// lastSeq is the last handled sequence. windowBase is the one before the
	// open window's first event.
	lastSeq     uint64
	windowBase  uint64
	windowStart time.Time
	buckets     map[string]*Digest
	errs        []error
	// held is set by the first window whose digests were not all delivered. The
	// cursor then stays at heldSeq, the sequence before that window, so that a
	// restart reads the window again and delivers it (at-least-once delivery).
	held    bool
	heldSeq uint64
	// savedSeq is the sequence the cursor holds: the loaded one (Options.After)
	// until the first save.
	savedSeq uint64
}

// New validates the options and returns a Superfan.
func New(opts Options) (*Superfan, error) {
	if opts.Config == nil {
		return nil, errors.New("superfan: no configuration")
	}
	cfg := opts.Config
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opts.Source == nil {
		return nil, errors.New("superfan: no event source")
	}
	analyzer, err := NewAnalyzer(cfg.Rules)
	if err != nil {
		return nil, err
	}
	// A sustained burst is reported once in every digest window it spans.
	analyzer.SetReportWindow(cfg.WindowDuration())
	notifiers := opts.Notifiers
	if notifiers == nil {
		stdout := opts.Stdout
		if stdout == nil {
			stdout = os.Stdout
		}
		if notifiers, err = cfg.BuildNotifiers(stdout); err != nil {
			return nil, err
		}
	}
	for _, r := range cfg.Routes {
		for _, n := range r.Notifiers {
			if notifiers[n] == nil {
				return nil, fmt.Errorf("superfan: no notifier named %q", n)
			}
		}
	}
	summarizer := opts.Summarizer
	if summarizer == nil {
		if summarizer, err = NewSummarizer(cfg.Summarizer.Type, cfg.Summarizer.Options); err != nil {
			return nil, err
		}
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	componentPaths := make(map[string][]string, len(cfg.Components))
	for _, c := range cfg.Components {
		componentPaths[c.Name] = c.Paths
	}
	return &Superfan{
		cfg:            cfg,
		src:            opts.Source,
		details:        opts.Details,
		notifiers:      notifiers,
		summarizer:     summarizer,
		cursor:         opts.Cursor,
		clock:          clock,
		tick:           opts.TickInterval,
		log:            logger,
		analyzer:       analyzer,
		window:         cfg.WindowDuration(),
		components:     cfg.Components,
		componentPaths: componentPaths,
		references:     cfg.References,
		routes:         cfg.Routes,
		lastSeq:        opts.After,
		savedSeq:       opts.After,
	}, nil
}

// Run consumes events until the source ends or ctx is done, then delivers the
// open window. It returns the delivery errors, if any. A read failure stops the
// run with an error after the events read so far were delivered. A cancelled
// context is a normal stop.
func (s *Superfan) Run(ctx context.Context) error {
	type item struct {
		ev  Event
		err error
	}
	items := make(chan item)
	readCtx, stopReading := context.WithCancel(ctx)
	defer stopReading()
	go func() {
		for {
			ev, err := s.src.Next(readCtx)
			select {
			case items <- item{ev: ev, err: err}:
			case <-readCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var ticks <-chan time.Time
	if s.tick > 0 {
		ticker := time.NewTicker(s.tick)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return s.finish(ctx)
		case <-ticks:
			// Without an open window, flush only saves the progress made by
			// filtered events, so that the cursor does not lag behind them.
			if s.windowStart.IsZero() || !s.clock().Before(s.windowStart.Add(s.window)) {
				s.flush(ctx)
			}
		case it := <-items:
			if errors.Is(it.err, io.EOF) {
				return s.finish(ctx)
			}
			if it.err != nil {
				if ctx.Err() != nil {
					return s.finish(ctx)
				}
				return errors.Join(fmt.Errorf("superfan: read events: %w", it.err), s.finish(ctx))
			}
			s.handle(ctx, it.ev)
		}
	}
}

// finish delivers the open window and reports the delivery errors. The
// deliveries are not cancelled by ctx, so that a shutdown still reports them.
func (s *Superfan) finish(ctx context.Context) error {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	s.flush(fctx)
	return errors.Join(s.errs...)
}

// handle applies the rules to one event and files the results.
//
// An event is handled only when its rules ran to the end. When the context is
// done, the event is left unhandled: it is neither filed nor counted in the
// cursor, so a restart reads it again. Otherwise a shutdown in the middle of a
// details fetch would skip its rules for good.
func (s *Superfan) handle(ctx context.Context, ev Event) {
	if ev.Sequence <= s.lastSeq {
		return
	}
	if ctx.Err() != nil {
		return
	}
	if len(s.references) > 0 && !matchAny(s.references, ev.Reference) {
		// Not a reference anyone asked for: handled, without a digest or a fetch.
		s.lastSeq = ev.Sequence
		return
	}

	// A batched land is judged change by change, each member with its own actor
	// and the details of its own commit (see batchUnits), as each of them was
	// before batching folded them into one changeset.
	units, complete := batchUnits(ev)
	changes := make([]inspected, 0, len(units))
	for _, u := range units {
		c, ok := s.inspect(ctx, u)
		if !ok {
			return
		}
		changes = append(changes, c)
	}
	if !complete {
		// The unlisted changes of the batch may touch any component.
		changes[len(changes)-1].uninspected = true
	}

	start := ev.RecordedAt.Truncate(s.window)
	if s.windowStart.IsZero() {
		s.windowStart = start
		s.windowBase = s.lastSeq
	} else if !ev.RecordedAt.Before(s.windowStart.Add(s.window)) {
		s.flush(ctx)
		s.windowStart = start
		s.windowBase = s.lastSeq
	}

	for _, c := range changes {
		findings := append(c.findings, s.analyzer.Analyze(c.ev, c.det)...)
		names := s.componentsFor(c.det)
		if c.uninspected {
			names = s.uninspectedComponents()
		}
		for _, name := range names {
			b := s.bucket(name)
			b.Events++
			b.addEntry(s.entryFor(name, c.ev, c.det))
			for _, f := range findings {
				f.Component = name
				b.addFinding(f)
			}
		}
	}
	s.lastSeq = ev.Sequence
}

// inspected is one change of an event (see batchUnits) with what is known
// about its commit.
type inspected struct {
	ev Event
	// det is the commit details, or nil when they are unknown.
	det *Details
	// findings are the findings about reading the details.
	findings []Finding
	// uninspected is set when the change has files that could not be read. It
	// may then belong to any component.
	uninspected bool
}

// inspect fetches the details of the commit of one change. ok is false when ctx
// is done: the event must then be left unhandled.
func (s *Superfan) inspect(ctx context.Context, ev Event) (c inspected, ok bool) {
	c.ev = ev
	// A deletion has no commit to read.
	if s.details == nil || isDeletion(ev) {
		return c, true
	}
	d, err := s.details.Details(ctx, ev)
	switch {
	case ctx.Err() != nil:
		return c, false
	case errors.Is(err, ErrDetailsTooLarge):
		// Too large to inspect is a large change: the files and lines of the
		// commit are over any limit, so it is reported as one.
		s.log.Warn("superfan: commit details too large", "sequence", ev.Sequence, "err", err)
		c.uninspected = true
		c.findings = append(c.findings, Finding{
			Rule:     RuleLargeChange,
			Severity: SeverityMedium,
			Summary:  "commit is too large to inspect: its details exceed the limit",
			Event:    ev,
		})
	case err != nil:
		// Without the details the protected-path and large-change rules cannot
		// run, so a missing answer is not a quiet one.
		s.log.Warn("superfan: commit details unavailable", "sequence", ev.Sequence, "err", err)
		c.uninspected = true
		c.findings = append(c.findings, Finding{
			Rule:     RuleDetailsUnavailable,
			Severity: SeverityMedium,
			Summary:  "commit details could not be read; protected-path and large-change rules did not run",
			Event:    ev,
		})
	default:
		c.det = &d
	}
	return c, true
}

// isDeletion reports whether the event deletes its reference. The new object
// ID is then the zero ID, and there is no commit to read.
func isDeletion(ev Event) bool {
	return strings.Trim(ev.NewOID, "0") == ""
}

// entryFor describes the changeset of an event for one component.
func (s *Superfan) entryFor(name string, ev Event, det *Details) DigestEntry {
	e := DigestEntry{
		Sequence:   ev.Sequence,
		Reference:  oneLine(ev.Reference, maxPathText),
		Operation:  oneLine(ev.Operation, maxLabelText),
		Actor:      oneLine(ev.Actor, maxLabelText),
		OID:        ev.NewOID,
		RecordedAt: ev.RecordedAt,
	}
	if det == nil {
		return e
	}
	e.Subject = oneLine(det.Subject, maxSubjectText)
	everything := name == UnassignedComponent
	for _, f := range det.Files {
		if !everything && !MatchFile(s.componentPaths[name], f.Path) {
			continue
		}
		if len(e.Files) < maxEntryFiles {
			e.Files = append(e.Files, oneLine(f.Path, maxPathText))
		} else {
			e.FilesOmitted++
		}
	}
	return e
}

// componentsFor returns the components whose paths the commit touches, sorted.
// A commit that touches none, or whose details are unknown, is unassigned.
func (s *Superfan) componentsFor(det *Details) []string {
	if det == nil {
		return []string{UnassignedComponent}
	}
	set := map[string]bool{}
	for _, comp := range s.components {
		for _, f := range det.Files {
			if MatchFile(comp.Paths, f.Path) {
				set[comp.Name] = true
				break
			}
		}
	}
	if len(set) == 0 {
		return []string{UnassignedComponent}
	}
	return sortedNames(set)
}

// uninspectedComponents returns where a changeset goes whose files could not
// be read (too large to inspect, or unreadable): UnassignedComponent and every
// component that a route names. Its files are unknown, so it may touch any
// component, and a commit made too large on purpose must not slip past the
// owners of a protected component whose route sends nothing else. Components
// without a route of their own are left out, since only "*" routes would see
// them, and those see UnassignedComponent already.
func (s *Superfan) uninspectedComponents() []string {
	set := map[string]bool{UnassignedComponent: true}
	for _, r := range s.routes {
		if _, ok := s.componentPaths[r.Component]; ok {
			set[r.Component] = true
		}
	}
	return sortedNames(set)
}

func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// bucket returns the digest of a component in the open window.
func (s *Superfan) bucket(name string) *Digest {
	if s.buckets == nil {
		s.buckets = map[string]*Digest{}
	}
	b := s.buckets[name]
	if b == nil {
		b = &Digest{
			Component: name,
			Start:     s.windowStart,
			End:       s.windowStart.Add(s.window),
			Risk:      SeverityNone,
		}
		s.buckets[name] = b
	}
	return b
}

// addEntry keeps the changeset description when there is room for it.
func (d *Digest) addEntry(e DigestEntry) {
	if len(d.Entries) < maxDigestEntries {
		d.Entries = append(d.Entries, e)
		return
	}
	d.EntriesOmitted++
}

// addFinding keeps the finding when there is room for it. The risk counts every
// finding.
func (d *Digest) addFinding(f Finding) {
	d.Risk = maxRisk(d.Risk, f.Severity)
	if len(d.Findings) < maxDigestItems {
		d.Findings = append(d.Findings, f)
		return
	}
	d.FindingsOmitted++
}

// flush delivers the digests of the open window, in component order, and then
// saves the cursor. When every digest was delivered, the saved cursor is the
// last handled sequence. A digest that a notifier failed to take holds the
// cursor at the sequence before the window, once and for the rest of the run:
// the window is read again after a restart, so it is delivered again (the
// digests of the other windows may then be repeated). A window without
// findings is delivered only when Digest.IncludeQuiet is set.
//
// Without an open window there is nothing to deliver, but the events filtered
// out by Config.References may still have moved the last handled sequence. That
// progress is saved too: otherwise a stream of only filtered events would never
// move the cursor, and a watch command that prints a bounded page of events
// after the cursor (`graft log --json --after {after}`) would read the same
// page forever.
func (s *Superfan) flush(ctx context.Context) {
	if s.windowStart.IsZero() {
		if s.cursorSeq() != s.savedSeq {
			s.saveCursor()
		}
		return
	}
	names := make([]string, 0, len(s.buckets))
	for n := range s.buckets {
		names = append(names, n)
	}
	sort.Strings(names)
	delivered := true
	for _, name := range names {
		if d := s.buckets[name]; len(d.Findings) > 0 || s.cfg.Digest.IncludeQuiet {
			if !s.deliver(ctx, *d) {
				delivered = false
			}
		}
	}
	if !delivered && !s.held {
		s.held = true
		s.heldSeq = s.windowBase
	}
	s.buckets = nil
	s.windowStart = time.Time{}
	s.saveCursor()
}

// cursorSeq returns the sequence the cursor must hold: the last handled one, or
// the held one after a failed delivery.
func (s *Superfan) cursorSeq() uint64 {
	if s.held {
		return s.heldSeq
	}
	return s.lastSeq
}

// saveCursor saves cursorSeq, when there is a cursor store.
func (s *Superfan) saveCursor() {
	if s.cursor == nil {
		return
	}
	seq := s.cursorSeq()
	if err := s.cursor.Save(seq); err != nil {
		s.fail(err)
		return
	}
	s.savedSeq = seq
}

// deliver summarizes one digest and sends it to the notifiers of its component.
// A summarizer failure is logged; the digest is sent without a summary. It
// reports whether every notifier of the component took the digest.
func (s *Superfan) deliver(ctx context.Context, d Digest) bool {
	msg := notify.Message{
		Kind:        notify.KindDigest,
		Component:   d.Component,
		WindowStart: d.Start,
		WindowEnd:   d.End,
		Events:      d.Events,
		Risk:        d.Risk,
		Omitted:     d.FindingsOmitted,
	}
	if s.summarizer != nil {
		sum, err := s.summarizer.Summarize(ctx, d)
		if err != nil {
			s.log.Warn("superfan: summary failed", "component", d.Component, "err", err)
		} else {
			msg.Summary = multiLine(sum.Text, maxSummaryBytes)
			if validRisk(sum.Risk) {
				msg.Risk = maxRisk(msg.Risk, sum.Risk)
			}
		}
	}
	for _, f := range d.Findings {
		msg.Findings = append(msg.Findings, notify.Finding{
			Rule:       f.Rule,
			Severity:   f.Severity,
			Summary:    f.Summary,
			Sequence:   f.Event.Sequence,
			Reference:  f.Event.Reference,
			Actor:      f.Event.Actor,
			OID:        f.Event.NewOID,
			RecordedAt: f.Event.RecordedAt,
		})
	}
	ok := true
	for _, name := range s.targets(d.Component) {
		if err := s.notifiers[name].Notify(ctx, msg); err != nil {
			s.fail(fmt.Errorf("superfan: notifier %q: %w", name, err))
			ok = false
		}
	}
	return ok
}

// targets returns the notifier names of the routes that apply to a component,
// in configuration order, without duplicates.
func (s *Superfan) targets(component string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range s.routes {
		if r.Component != AnyComponent && r.Component != component {
			continue
		}
		for _, n := range r.Notifiers {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

func (s *Superfan) fail(err error) {
	s.log.Error(err.Error())
	if len(s.errs) < maxRecordedErrors {
		s.errs = append(s.errs, err)
	}
}

// truncateText cuts s to at most max bytes, on a UTF-8 boundary.
func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}
