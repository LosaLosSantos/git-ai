package bench

import (
	"context"
	"errors"

	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/syntax"
)

// SyntaxFinding is a human resolution that fails the syntax check of the llm answers.
type SyntaxFinding struct {
	ID       string `json:"id"`
	Repo     string `json:"repo"`
	Merge    string `json:"merge"`
	Path     string `json:"path"`
	Language string `json:"language"`
	Reason   string `json:"reason"`
	// Markers says the human resolution still has conflict markers.
	Markers bool `json:"markers"`
	// Unchecked marks a case the check gives no verdict on: Reason then names the side that does
	// not parse and its first error.
	Unchecked bool `json:"unchecked,omitempty"`
}

// SyntaxStats counts, per language, how the syntax check of the llm answers (syntax.CheckMerge)
// treats the human resolutions: Checked when both sides parse, of which Failed do not parse
// themselves (a check rejecting a human resolution would reject a correct AI one just the same,
// unless the human one is broken); Unchecked when a side does not parse, so the check gives no
// verdict on the file.
type SyntaxStats struct {
	Checked   int `json:"checked"`
	Failed    int `json:"failed"`
	Unchecked int `json:"unchecked"`
}

// HumanSyntax runs the syntax check of the llm answers on the human resolution of every case
// (a deleted file has none), with both sides, and returns the counts by language (LanguageOf),
// every failure, and every case without a verdict. Paths with no grammar are not counted.
func HumanSyntax(ctx context.Context, cases []Case, clone func(repo string) string) (map[string]SyntaxStats, []SyntaxFinding, error) {
	stats := map[string]SyntaxStats{}
	var findings []SyntaxFinding
	for _, c := range cases {
		if c.HumanBlob == nil {
			continue
		}
		if _, ok := syntax.LanguageOf(c.Path); !ok {
			continue
		}
		g := gitRepo{dir: clone(c.Repo)}
		var blobs [3][]byte
		for i, e := range []*entry{c.HumanBlob, c.OursBlob, c.TheirsBlob} {
			if e == nil {
				continue
			}
			b, err := g.blob(ctx, e.OID)
			if err != nil {
				return nil, nil, err
			}
			blobs[i] = b
		}
		lang := LanguageOf(c.Path)
		s := stats[lang]
		checked, err := syntax.CheckMerge(ctx, c.Path, blobs[0], blobs[1], blobs[2])
		var se *syntax.Error
		switch {
		case !checked && err == nil:
			s.Unchecked++
			// Name the side the grammar does not parse, and where.
			f := SyntaxFinding{ID: c.ID, Repo: c.Repo, Merge: c.Merge, Path: c.Path, Language: lang, Unchecked: true}
			for i, side := range []string{"ours", "theirs"} {
				if blobs[i+1] == nil {
					continue
				}
				if err := syntax.Check(ctx, c.Path, blobs[i+1]); err != nil {
					f.Reason = side + ": " + err.Error()
					break
				}
			}
			findings = append(findings, f)
		case err == nil:
			s.Checked++
		case errors.As(err, &se):
			s.Checked++
			s.Failed++
			findings = append(findings, SyntaxFinding{ID: c.ID, Repo: c.Repo, Merge: c.Merge, Path: c.Path, Language: lang,
				Reason: se.Error(), Markers: hasMarkers(blobs[0])})
		default:
			return nil, nil, err
		}
		stats[lang] = s
	}
	return stats, findings, nil
}
