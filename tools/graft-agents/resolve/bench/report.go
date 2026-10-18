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
	// HumanSyntax counts the cases whose human resolution fails the syntax check.
	HumanSyntax int `json:"human_fails_syntax"`
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
	if r.HumanSyntaxError != "" {
		c.HumanSyntax++
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

// NewReport aggregates results.
func NewReport(label string, results []Result) Report {
	rep := Report{Label: label, Families: map[string]Counts{}, NotSent: map[string]int{}, Results: results}
	for _, r := range results {
		rep.Total.add(r)
		fc := rep.Families[r.Family]
		fc.add(r)
		rep.Families[r.Family] = fc
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
			p.Prompt*1e6, (p.Completion+p.Reasoning)*1e6, p.Completion*1e6, p.Reasoning*1e6, p.Request)
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
	b.WriteString("\nThe error rate is wrong / (correct + wrong): the share of the results that would land and differ from the human resolution. Correct means equal after normalizing line endings and trailing whitespace; exact means byte-identical.\n")
	syntaxParts := make([]string, 0, len(families))
	for _, f := range families {
		syntaxParts = append(syntaxParts, fmt.Sprintf("%s %d", f, rep.Families[f].HumanSyntax))
	}
	fmt.Fprintf(&b, "\nHuman resolutions that fail the syntax check themselves, where the check would reject the right answer: %d (%s).\n",
		rep.Total.HumanSyntax, strings.Join(syntaxParts, ", "))
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
