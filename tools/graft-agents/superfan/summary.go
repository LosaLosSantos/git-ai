package superfan

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// summarizerNone is the default summarizer: digests carry no summary.
const summarizerNone = "none"

// Summary is the answer of a summarizer for one digest.
type Summary struct {
	// Text is a concise summary of the digest.
	Text string
	// Risk is an optional risk level. It can raise the deterministic risk of
	// the digest, never lower it. Invalid values are ignored.
	Risk string
}

// Summarizer turns a digest into a summary. Implementations are optional and
// may call a language model (package tools/graft-agents/superfan, summarize*.go).
// The digest handed over is bounded: at most maxDigestItems findings and
// maxDigestEntries changesets, each with its text cut to a few hundred bytes
// and stripped of control characters. An implementation that sends the digest
// to a model should still cap what it sends.
type Summarizer interface {
	Summarize(ctx context.Context, d Digest) (Summary, error)
}

// SummarizerFactory builds a summarizer from the options of its configuration.
type SummarizerFactory func(options json.RawMessage) (Summarizer, error)

var (
	summarizersMu sync.RWMutex
	summarizers   = map[string]SummarizerFactory{
		// "none" returns a nil Summarizer: no summaries are made.
		summarizerNone: func(json.RawMessage) (Summarizer, error) { return nil, nil },
	}
)

// RegisterSummarizer makes a summarizer type available to the configuration. It
// is meant to be called from init. Registering a name twice panics.
func RegisterSummarizer(name string, f SummarizerFactory) {
	summarizersMu.Lock()
	defer summarizersMu.Unlock()
	if name == "" || f == nil {
		panic("superfan: RegisterSummarizer needs a name and a factory")
	}
	if _, dup := summarizers[name]; dup {
		panic(fmt.Sprintf("superfan: summarizer %q registered twice", name))
	}
	summarizers[name] = f
}

// SummarizerTypes returns the registered summarizer types, sorted.
func SummarizerTypes() []string {
	summarizersMu.RLock()
	defer summarizersMu.RUnlock()
	names := make([]string, 0, len(summarizers))
	for n := range summarizers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func isSummarizerRegistered(name string) bool {
	summarizersMu.RLock()
	defer summarizersMu.RUnlock()
	_, ok := summarizers[name]
	return ok
}

// NewSummarizer builds the summarizer of a type. An empty type means "none",
// which returns a nil Summarizer.
func NewSummarizer(typ string, options json.RawMessage) (Summarizer, error) {
	if typ == "" {
		typ = summarizerNone
	}
	summarizersMu.RLock()
	f, ok := summarizers[typ]
	summarizersMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("superfan: unknown summarizer %q", typ)
	}
	return f(options)
}

// riskRank orders risk levels; unknown levels rank as none.
func riskRank(risk string) int {
	switch risk {
	case SeverityLow:
		return 1
	case SeverityMedium:
		return 2
	case SeverityHigh:
		return 3
	default:
		return 0
	}
}

// validRisk reports whether risk is a level a summary may set.
func validRisk(risk string) bool {
	switch risk {
	case SeverityNone, SeverityLow, SeverityMedium, SeverityHigh:
		return true
	}
	return false
}

// maxRisk returns the higher of two risk levels.
func maxRisk(a, b string) string {
	if riskRank(b) > riskRank(a) {
		return b
	}
	if a == "" {
		return SeverityNone
	}
	return a
}
