package bench

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/tools/graft-agents/resolve"
	"github.com/feder-cr/git-ai/tools/graft-agents/resolve/syntax"
)

// Reasons a human resolution is not a valid reference. A merge commit is not always a correct
// resolution: some were committed with the conflict markers still in the file, or broke the
// syntax and were fixed in a later commit. Judged against such a reference, a correct answer
// would count as wrong, so such cases are left out of the corpus (and counted).
const (
	InvalidMarkers = "the human resolution contains conflict markers"
	InvalidSyntax  = "the human resolution does not parse although both sides do"
)

// invalidReference returns why the human resolution of c is not a valid reference, or "" when it
// is. It reads the blobs of the human resolution and both sides.
func invalidReference(ctx context.Context, g gitRepo, c Case) (string, error) {
	if c.HumanBlob == nil {
		return "", nil // a deletion
	}
	human, err := g.blob(ctx, c.HumanBlob.OID)
	if err != nil {
		return "", err
	}
	if hasMarkers(human) {
		return InvalidMarkers, nil
	}
	var sides [][]byte
	for _, e := range []*entry{c.OursBlob, c.TheirsBlob} {
		if e == nil {
			continue
		}
		b, err := g.blob(ctx, e.OID)
		if err != nil {
			return "", err
		}
		sides = append(sides, b)
	}
	checked, err := syntax.CheckMerge(ctx, c.Path, human, sides...)
	var se *syntax.Error
	switch {
	case errors.As(err, &se) && checked:
		return InvalidSyntax, nil
	case err != nil:
		return "", fmt.Errorf("checking the reference of %s: %w", c.Path, err)
	}
	return "", nil
}

// FilterStats counts what Filter left out, by reason.
type FilterStats map[string]int

// Filter returns the cases of a manifest whose human resolution is a valid reference, with the
// cases left out counted by reason. Mine applies the same rule to the cases it keeps; Filter
// applies it to a manifest mined before the rule.
func Filter(ctx context.Context, cases []Case, clone func(repo string) string) ([]Case, FilterStats, error) {
	if !syntax.Available {
		return nil, nil, syntax.ErrUnavailable
	}
	stats := FilterStats{}
	var kept []Case
	for _, c := range cases {
		reason, err := invalidReference(ctx, gitRepo{dir: clone(c.Repo)}, c)
		if err != nil {
			return nil, nil, fmt.Errorf("case %s: %w", c.ID, err)
		}
		if reason != "" {
			stats[c.Repo+": "+reason]++
			continue
		}
		kept = append(kept, c)
	}
	return kept, stats, nil
}

// hasMarkers is the marker rule of the resolver answers (resolve.HasConflictMarkers).
func hasMarkers(b []byte) bool { return resolve.HasConflictMarkers(b) }
