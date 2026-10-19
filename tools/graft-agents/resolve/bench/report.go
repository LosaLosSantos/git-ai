package bench

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Counts aggregates the outcomes of a set of cases.
type Counts struct {
	Cases     int `json:"cases"`
	Correct   int `json:"correct"`
	Exact     int `json:"exact"`
	Wrong     int `json:"wrong"`
	Rejected  int `json:"rejected_by_checks"`
	NotSent   int `json:"not_sent"`
	Model     int `json:"model_error"`
	NotRun    int `json:"not_run_cap"`
	Abstained int `json:"abstained"`
	// BlankLinesOnly counts the wrong results that differ from the human resolution in blank
	// lines only (part of Wrong); SyntaxUnchecked the accepted results the syntax check gave no
	// verdict on, because a side does not parse.
	BlankLinesOnly  int `json:"wrong_blank_lines_only"`
	SyntaxUnchecked int `json:"syntax_unchecked"`
}

// Accepted is the number of results that would land: correct and wrong.
func (c Counts) Accepted() int { return c.Correct + c.Wrong }

// ErrorRate is wrong / accepted, the share of the landed AI resolutions that differ from the
// human one; 0 when nothing was accepted.
func (c Counts) ErrorRate() float64 {
	if c.Accepted() == 0 {
		return 0
	}
	return float64(c.Wrong) / float64(c.Accepted())
}

func (c *Counts) add(r Result) {
	c.Cases++
	if r.BlankLinesOnly {
		c.BlankLinesOnly++
	}
	if r.SyntaxUnchecked {
		c.SyntaxUnchecked++
	}
	switch r.Outcome {
	case OutcomeCorrect:
		c.Correct++
		if r.Exact {
			c.Exact++
		}
	case OutcomeWrong:
		c.Wrong++
	case OutcomeRejected:
		c.Rejected++
	case OutcomeNotSent:
		c.NotSent++
	case OutcomeModelError:
		c.Model++
	case OutcomeNotRun:
		c.NotRun++
	case OutcomeAbstained:
		c.Abstained++
	}
}

// Report is the outcome of a run: the totals, the counts per family and every result.
type Report struct {
	// Label names the run, for example the model.
	Label    string            `json:"label"`
	Total    Counts            `json:"total"`
	Families map[string]Counts `json:"families"`
	// Languages and Kinds count the same results by language (bench.LanguageOf) and by kind of
	// conflict: the llm strategy declines a modify-delete file, so the content row is what the
	// model attempted.
	Languages map[string]Counts `json:"languages"`
	Kinds     map[string]Counts `json:"kinds"`
	// NotSent counts the cases that were not sent, by reason.
	NotSent map[string]int `json:"not_sent"`
	// SpendUSD is the AI spend of the run in USD, when a budget was used: the usage the provider
	// reported, and the worst case of the calls whose usage was not reported. MaxUSD is the cap.
	SpendUSD float64 `json:"spend_usd,omitempty"`
	MaxUSD   float64 `json:"max_usd,omitempty"`
	// Pricing is the price the spend was computed with, as read at run time.
	Pricing *Pricing `json:"pricing,omitempty"`
	Results []Result `json:"results"`
}

// LowN is the number of cases below which a row is marked as low-n in the Markdown report.
const LowN = 30

// NewReport aggregates results.
func NewReport(label string, results []Result) Report {
	rep := Report{Label: label, Families: map[string]Counts{}, Languages: map[string]Counts{}, Kinds: map[string]Counts{},
		NotSent: map[string]int{}, Results: results}
	for _, r := range results {
		rep.Total.add(r)
		for _, g := range []struct {
			m   map[string]Counts
			key string
		}{{rep.Families, r.Family}, {rep.Languages, r.Language}, {rep.Kinds, r.Kind}} {
			c := g.m[g.key]
			c.add(r)
			g.m[g.key] = c
		}
		if r.Outcome == OutcomeNotSent {
			rep.NotSent[r.Reason]++
		}
	}
	return rep
}

// WriteMarkdown writes the report as Markdown tables.
func (rep Report) WriteMarkdown(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "## graft-resolve bench: %s\n\n", rep.Label)
	fmt.Fprintf(&b, "Cases sampled: %d; run: %d", rep.Total.Cases, rep.Total.Cases-rep.Total.NotRun)
	if rep.Total.NotRun > 0 {
		fmt.Fprintf(&b, " (%d not run: the cap was reached)", rep.Total.NotRun)
	}
	if rep.MaxUSD > 0 {
		fmt.Fprintf(&b, "; AI spend $%.4f of the $%.2f cap", rep.SpendUSD, rep.MaxUSD)
	}
	b.WriteString(".\n\n")
	if p := rep.Pricing; p != nil {
		fmt.Fprintf(&b, "Model %s, priced at run time (%s, %s, highest of %d endpoints): $%g per million input tokens, $%g per million output tokens (completion $%g + internal reasoning $%g), $%g per request.\n\n",
			p.Model, p.Source, p.FetchedAt.Format(time.RFC3339), p.Endpoints,
			PerMillion(p.Prompt), PerMillion(p.Completion, p.Reasoning), PerMillion(p.Completion), PerMillion(p.Reasoning), p.Request)
	}
	b.WriteString("| family | cases | correct (exact) | wrong | rejected by checks | not sent | model error | not run (cap) | abstained | error rate |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	families := make([]string, 0, len(rep.Families))
	for f := range rep.Families {
		families = append(families, f)
	}
	sort.Strings(families)
	row := func(name string, c Counts) {
		fmt.Fprintf(&b, "| %s | %d | %d (%d) | %d | %d | %d | %d | %d | %d | %.1f%% |\n",
			name, c.Cases, c.Correct, c.Exact, c.Wrong, c.Rejected, c.NotSent, c.Model, c.NotRun, c.Abstained, 100*c.ErrorRate())
	}
	for _, f := range families {
		row(f, rep.Families[f])
	}
	row("total", rep.Total)

	// The same results by language and by kind of conflict.
	for _, group := range []struct {
		title string
		m     map[string]Counts
	}{{"language", rep.Languages}, {"kind", rep.Kinds}} {
		if len(group.m) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n| %s | cases | correct (exact) | wrong | rejected by checks | not sent | model error | not run (cap) | abstained | error rate |\n", group.title)
		b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
		keys := make([]string, 0, len(group.m))
		for k := range group.m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			name := k
			if name == "" {
				name = "other"
			}
			if group.m[k].Cases < LowN {
				name += " (low n)"
			}
			row(name, group.m[k])
		}
	}
	fmt.Fprintf(&b, "\nRows marked low n have fewer than %d cases; their rates are not significant. The llm strategy declines a modify-delete or mode conflict (a side is absent), which goes to the agent as abstained: the content row is what the model attempted.\n", LowN)
	b.WriteString("\nThe error rate is wrong / (correct + wrong): the share of the results that would land and differ from the human resolution. Correct means equal after normalizing line endings and trailing whitespace; exact means byte-identical.\n")
	perFamily := func(value func(Counts) int) string {
		parts := make([]string, 0, len(families))
		for _, f := range families {
			parts = append(parts, fmt.Sprintf("%s %d", f, value(rep.Families[f])))
		}
		return strings.Join(parts, ", ")
	}
	fmt.Fprintf(&b, "\nWrong results that differ from the human resolution in blank lines only (counted as wrong): %d (%s).\n",
		rep.Total.BlankLinesOnly, perFamily(func(c Counts) int { return c.BlankLinesOnly }))
	languages := make([]string, 0, len(rep.Languages))
	for l := range rep.Languages {
		languages = append(languages, l)
	}
	sort.Strings(languages)
	var unchecked []string
	for _, l := range languages {
		c := rep.Languages[l]
		if l == "" {
			continue
		}
		unchecked = append(unchecked, fmt.Sprintf("%s %d of %d accepted", l, c.SyntaxUnchecked, c.Accepted()))
	}
	fmt.Fprintf(&b, "\nAccepted results the syntax check gave no verdict on, because a side does not parse with the grammar (a dialect such as Flow, or code the grammar does not cover): %d of %d. By language: %s.\n",
		rep.Total.SyntaxUnchecked, rep.Total.Accepted(), strings.Join(unchecked, ", "))
	if len(rep.NotSent) > 0 {
		reasons := make([]string, 0, len(rep.NotSent))
		for r := range rep.NotSent {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		parts := make([]string, 0, len(reasons))
		for _, r := range reasons {
			parts = append(parts, fmt.Sprintf("%s %d", r, rep.NotSent[r]))
		}
		fmt.Fprintf(&b, "\nNot sent, by reason: %s.\n", strings.Join(parts, ", "))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
