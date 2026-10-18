package bench

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
)

// Outcomes of a case.
const (
	// OutcomeCorrect: the chain resolved the file and the result equals the human resolution.
	OutcomeCorrect = "correct"
	// OutcomeWrong: the chain resolved the file, the checks accepted it, and it differs from the
	// human resolution. A wrong answer lands without review.
	OutcomeWrong = "wrong"
	// OutcomeRejected: a result was rejected by the rules (markers, mode, size, syntax) or a
	// configured check; the land would fail and the agent would get the package.
	OutcomeRejected = "rejected_by_checks"
	// OutcomeNotSent: the file is never sent to the resolver (binary, large, lockfile, generated,
	// minified).
	OutcomeNotSent = "not_sent"
	// OutcomeModelError: a strategy failed (for a model, the call or its answer).
	OutcomeModelError = "model_error"
	// OutcomeNotRun: the AI budget of the run was spent before the case.
	OutcomeNotRun = "not_run_cap"
	// OutcomeAbstained: no strategy applied to the file.
	OutcomeAbstained = "abstained"
)

// Result is the outcome of one case.
type Result struct {
	ID       string  `json:"id"`
	Repo     string  `json:"repo"`
	Family   string  `json:"family"`
	Path     string  `json:"path"`
	Outcome  string  `json:"outcome"`
	Exact    bool    `json:"exact,omitempty"`
	Strategy string  `json:"strategy,omitempty"`
	Reason   string  `json:"reason,omitempty"`
	Seconds  float64 `json:"seconds"`
}

// Sample returns n cases of cases, stratified by family: each family's cases are shuffled with the
// seed and the families are taken in turn, so that the sample is the same for the same manifest and
// seed. n <= 0 takes every case.
func Sample(cases []Case, n int, seed int64) []Case {
	byFamily := map[string][]Case{}
	for _, c := range cases {
		byFamily[c.Family] = append(byFamily[c.Family], c)
	}
	families := make([]string, 0, len(byFamily))
	for f := range byFamily {
		families = append(families, f)
	}
	sort.Strings(families)

	rnd := rand.New(rand.NewSource(seed))
	for _, f := range families {
		group := byFamily[f]
		sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
		rnd.Shuffle(len(group), func(i, j int) { group[i], group[j] = group[j], group[i] })
	}

	if n <= 0 {
		n = len(cases)
	}
	var sample []Case
	for len(sample) < n {
		took := false
		for _, f := range families {
			if len(byFamily[f]) == 0 || len(sample) >= n {
				continue
			}
			sample = append(sample, byFamily[f][0])
			byFamily[f] = byFamily[f][1:]
			took = true
		}
		if !took {
			break
		}
	}
	return sample
}

// Runner replays cases through a resolver.
type Runner struct {
	Resolver *resolve.Resolver
	// Clone returns the directory of the clone of a corpus repository.
	Clone func(repo string) string
	// MaxFileBytes is the server's max_file_bytes (protocol.DefaultMaxFileBytes when 0).
	MaxFileBytes int64
	// Progress, when set, is called after each case.
	Progress func(done, total int, r Result)
}

// Run replays every case and returns the results in the order of cases.
func (r Runner) Run(ctx context.Context, cases []Case) ([]Result, error) {
	maxBytes := r.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = protocol.DefaultMaxFileBytes
	}

	results := make([]Result, 0, len(cases))
	for i, c := range cases {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		res, err := r.runCase(ctx, c, maxBytes)
		if err != nil {
			return results, fmt.Errorf("case %s (%s %s): %w", c.ID, c.Repo, c.Path, err)
		}
		results = append(results, res)
		if r.Progress != nil {
			r.Progress(i+1, len(cases), res)
		}
	}
	return results, nil
}

func (r Runner) runCase(ctx context.Context, c Case, maxBytes int64) (Result, error) {
	res := Result{ID: c.ID, Repo: c.Repo, Family: c.Family, Path: c.Path}
	g := gitRepo{dir: r.Clone(c.Repo)}

	sides := map[string]*protocol.Side{}
	var contents [][]byte
	for name, e := range map[string]*entry{"base": c.BaseBlob, "ours": c.OursBlob, "theirs": c.TheirsBlob} {
		if e == nil {
			continue
		}
		content, err := g.blob(ctx, e.OID)
		if err != nil {
			return res, err
		}
		sides[name] = &protocol.Side{OID: e.OID, Mode: e.Mode, Content: content}
		contents = append(contents, content)
	}
	var human []byte
	if c.HumanBlob != nil {
		var err error
		if human, err = g.blob(ctx, c.HumanBlob.OID); err != nil {
			return res, err
		}
	}

	generated, err := g.linguistGenerated(ctx, c.Ours, c.Path)
	if err != nil {
		return res, err
	}
	if reason := notSentReason(c.Path, contents, maxBytes, generated); reason != "" {
		res.Outcome, res.Reason = OutcomeNotSent, reason
		return res, nil
	}

	oursMessage, err := g.message(ctx, c.Ours)
	if err != nil {
		return res, err
	}
	theirsMessage, err := g.message(ctx, c.Theirs)
	if err != nil {
		return res, err
	}

	req := protocol.Request{
		Version:    protocol.Version,
		Repository: protocol.Repository{Storage: "bench", RelativePath: c.Repo},
		Branch:     "bench",
		Tip:        c.Ours,
		Commit:     c.Theirs,
		MergeBase:  c.Base,
		Messages:   protocol.Messages{Tip: oursMessage, Commit: theirsMessage},
		Actor:      "bench",
		Files: []protocol.FileConflict{{
			Path: c.Path, Kind: c.Kind, Base: sides["base"], Ours: sides["ours"], Theirs: sides["theirs"],
		}},
		Limits: protocol.Limits{MaxFileBytes: maxBytes},
	}

	start := time.Now()
	resp, err := r.Resolver.Resolve(ctx, req)
	res.Seconds = time.Since(start).Seconds()
	if err != nil {
		var unresolved *resolve.UnresolvedError
		if !errors.As(err, &unresolved) || len(unresolved.Failures) != 1 {
			return res, err
		}
		res.Outcome, res.Strategy, res.Reason = classifyFailure(unresolved.Failures[0])
		return res, nil
	}

	f := resp.Files[0]
	res.Strategy = strings.TrimPrefix(resp.Resolver, "graft-resolve/")
	correct, exact := judge(f.Content, human, f.Action == protocol.ActionDelete, c.HumanBlob == nil)
	res.Exact = exact
	if correct {
		res.Outcome = OutcomeCorrect
	} else {
		res.Outcome = OutcomeWrong
	}
	return res, nil
}

// classifyFailure returns the outcome of a file that the chain did not resolve: the last attempt
// decides, since the chain went on past the earlier ones.
func classifyFailure(f resolve.Failure) (outcome, strategy, reason string) {
	if len(f.Attempts) == 0 {
		return OutcomeAbstained, "", strings.Join(f.Reasons, "; ")
	}
	last := f.Attempts[len(f.Attempts)-1]
	reason = truncate(fmt.Sprint(last.Err), 512)
	switch {
	case errors.Is(last.Err, resolve.ErrBudgetExhausted):
		return OutcomeNotRun, last.Strategy, reason
	case last.Kind == resolve.AttemptRejected || last.Kind == resolve.AttemptCheckFailed:
		return OutcomeRejected, last.Strategy, reason
	}
	return OutcomeModelError, last.Strategy, reason
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// linguistGenerated reports whether the .gitattributes of rev's tree mark path as
// linguist-generated.
func (g gitRepo) linguistGenerated(ctx context.Context, rev, p string) (bool, error) {
	out, _, err := g.run(ctx, nil, nil, "check-attr", "--source", rev, "linguist-generated", "--", p)
	if err != nil {
		return false, err
	}
	// "<path>: linguist-generated: <value>"
	value := strings.TrimSpace(string(out[strings.LastIndex(string(out), ":")+1:]))
	return value == "set" || value == "true", nil
}
