package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/syntax"
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
	// OutcomeStopped: the run stopped before the case (Runner.Stop), for example because the
	// provider refused a call for lack of credit; the reason is the result's.
	OutcomeStopped = "not_run_stopped"
	// OutcomeAbstained: no strategy applied to the file.
	OutcomeAbstained = "abstained"
)

// Result is the outcome of one case.
type Result struct {
	ID       string `json:"id"`
	Repo     string `json:"repo"`
	Family   string `json:"family"`
	Language string `json:"language"`
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Outcome  string `json:"outcome"`
	Exact    bool   `json:"exact,omitempty"`
	// BlankLinesOnly marks a wrong result that differs from the human resolution in blank lines
	// only; it is still wrong, and the report counts it apart.
	BlankLinesOnly bool `json:"blank_lines_only,omitempty"`
	// SyntaxUnchecked marks an accepted resolution the syntax check gave no verdict on: a side
	// does not parse, so the grammar cannot judge the file (syntax.CheckMerge).
	SyntaxUnchecked bool `json:"syntax_unchecked,omitempty"`
	// EqualsOurs marks an accepted resolution equal to OURS after the judge's normalization:
	// taking ours is often the right answer, and the report counts how often the model did.
	EqualsOurs bool   `json:"equals_ours,omitempty"`
	Strategy   string `json:"strategy,omitempty"`
	// Answer is the action of an accepted resolution ("write" or "delete"); with
	// Runner.AnswersDir its content is stored for audit and Rejudge.
	Answer  string  `json:"answer,omitempty"`
	Reason  string  `json:"reason,omitempty"`
	Seconds float64 `json:"seconds"`
}

// Sample returns n cases of cases, stratified by family: each family's cases are shuffled with the
// seed and the families are taken in turn, so that the sample is the same for the same manifest and
// seed. n <= 0 takes every case.
//
// The strata are the families, with Rust and Java apart (stratumOf): a stratum with fewer cases
// than its share is taken whole, and the rest is split evenly over the others.
func Sample(cases []Case, n int, seed int64) []Case {
	byFamily := map[string][]Case{}
	for _, c := range cases {
		s := stratumOf(c)
		byFamily[s] = append(byFamily[s], c)
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
	// AnswersDir, when set, keeps every accepted resolution: <AnswersDir>/<case ID>.answer holds
	// the resolved content (no file for a deletion), so that the judge can be audited and the
	// same answers judged again (Rejudge) without new model calls. The directory holds
	// third-party code and stays local.
	AnswersDir string
	// Stop, when set, is asked before each case whether the run must stop. A reason stops it:
	// that case and every case after it are not run, with outcome stopped and that reason.
	Stop func() string
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
		if r.Stop != nil {
			if reason := r.Stop(); reason != "" {
				for j, rest := range cases[i:] {
					res := Result{ID: rest.ID, Repo: rest.Repo, Family: rest.Family, Language: LanguageOf(rest.Path), Kind: rest.Kind, Path: rest.Path,
						Outcome: OutcomeStopped, Reason: reason}
					results = append(results, res)
					if r.Progress != nil {
						r.Progress(i+j+1, len(cases), res)
					}
				}
				return results, nil
			}
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
	res := Result{ID: c.ID, Repo: c.Repo, Family: c.Family, Language: LanguageOf(c.Path), Kind: c.Kind, Path: c.Path}
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
		// A content conflict with more regions than the llm strategy sends is declined before
		// any call: it is not sent, for that reason.
		if res.Outcome == OutcomeAbstained && c.Kind == protocol.KindContent && sides["ours"] != nil && sides["theirs"] != nil {
			if max := r.Resolver.LLMMaxRegions(); max > 0 && resolve.LLMConflictRegions(sideBytes(sides["base"]), sideBytes(sides["ours"]), sideBytes(sides["theirs"])) > max {
				res.Outcome, res.Reason = OutcomeNotSent, NotSentRegions
			}
		}
		return res, nil
	}

	f := resp.Files[0]
	res.Strategy = strings.TrimPrefix(resp.Resolver, "graft-resolve/")
	res.Answer = f.Action
	if r.AnswersDir != "" && f.Action != protocol.ActionDelete {
		if err := os.WriteFile(answerPath(r.AnswersDir, c.ID), f.Content, 0o600); err != nil {
			return res, err
		}
	}
	// A symbolic link has no syntax: its verdict is none, as in the check operation, never
	// unchecked.
	if f.Action == protocol.ActionWrite && f.Mode != protocol.ModeSymlink {
		if _, ok := syntax.LanguageOf(c.Path); ok {
			checked, err := syntax.CheckMerge(ctx, c.Path, f.Content, sideBytes(sides["ours"]), sideBytes(sides["theirs"]))
			if err != nil && ctx.Err() != nil {
				return res, ctx.Err()
			}
			res.SyntaxUnchecked = !checked
		}
	}
	judgeResult(&res, f.Content, f.Action == protocol.ActionDelete, human, c.HumanBlob == nil)
	res.EqualsOurs = f.Action == protocol.ActionWrite && sides["ours"] != nil && bytes.Equal(normalize(f.Content), normalize(sides["ours"].Content))
	return res, nil
}

// sideBytes returns the content of a present side, nil for an absent one.
func sideBytes(s *protocol.Side) []byte {
	if s == nil {
		return nil
	}
	if s.Content == nil {
		return []byte{}
	}
	return s.Content
}

func answerPath(dir, id string) string { return filepath.Join(dir, id+".answer") }

// judgeResult sets the outcome of an accepted resolution from the judge.
func judgeResult(res *Result, content []byte, deleted bool, human []byte, humanDeleted bool) {
	correct, exact := judge(content, human, deleted, humanDeleted)
	res.Exact = exact
	res.BlankLinesOnly = false
	if correct {
		res.Outcome = OutcomeCorrect
	} else {
		res.Outcome = OutcomeWrong
		res.BlankLinesOnly = !deleted && !humanDeleted && blankLinesOnly(content, human)
	}
}

// Rejudge judges the stored answers of results again (Runner.AnswersDir), with no model call:
// the outcome of every accepted resolution (correct or wrong) is recomputed from its stored
// answer and the human resolution; every other result is kept as it is. It is how a fix of the
// judge is applied to a run that was already paid for.
func Rejudge(ctx context.Context, results []Result, cases []Case, clone func(repo string) string, answersDir string) ([]Result, error) {
	byID := make(map[string]Case, len(cases))
	for _, c := range cases {
		byID[c.ID] = c
	}
	out := make([]Result, len(results))
	for i, res := range results {
		out[i] = res
		if res.Outcome != OutcomeCorrect && res.Outcome != OutcomeWrong {
			continue
		}
		c, ok := byID[res.ID]
		if !ok {
			return nil, fmt.Errorf("rejudge: case %s is not in the manifest", res.ID)
		}
		if res.Answer == "" {
			return nil, fmt.Errorf("rejudge: case %s has no stored answer (the run kept no answers)", res.ID)
		}
		g := gitRepo{dir: clone(c.Repo)}
		var human []byte
		if c.HumanBlob != nil {
			var err error
			if human, err = g.blob(ctx, c.HumanBlob.OID); err != nil {
				return nil, err
			}
		}
		deleted := res.Answer == protocol.ActionDelete
		var content []byte
		if !deleted {
			var err error
			if content, err = os.ReadFile(answerPath(answersDir, res.ID)); err != nil {
				return nil, fmt.Errorf("rejudge: %w", err)
			}
		}
		judgeResult(&out[i], content, deleted, human, c.HumanBlob == nil)
	}
	return out, nil
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
