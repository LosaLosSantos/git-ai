package bench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

// DefaultMaxMerges bounds the merges replayed per repository when the corpus sets no bound.
const DefaultMaxMerges = 3000

// MineOptions bound the mining of one repository.
type MineOptions struct {
	// MaxCases bounds the cases kept per repository, newest merges first (0: no bound).
	MaxCases int
}

// MineStats counts what the miner saw in one repository.
type MineStats struct {
	Merges            int `json:"merges"`
	ConflictingMerges int `json:"conflicting_merges"`
	// SkippedMergeBases counts merges whose parents have no single merge base (criss-cross or
	// unrelated histories): the resolver is given one base.
	SkippedMergeBases int `json:"skipped_merge_bases"`
	ConflictingFiles  int `json:"conflicting_files"`
	// OtherFamily counts conflicting files of no benchmark family.
	OtherFamily int `json:"other_family"`
	// OneSide counts conflicting files whose human resolution is one of the two sides, which
	// measures nothing about merging.
	OneSide int `json:"one_side"`
	Kept    int `json:"kept"`
}

// caseID returns the stable ID of a case.
func caseID(repo, merge, p string) string {
	sum := sha256.Sum256([]byte(repo + "\x00" + merge + "\x00" + p))
	return hex.EncodeToString(sum[:8])
}

// Mine replays the merges of repo, cloned at dir, and returns the cases: the conflicting files of
// a benchmark family whose human resolution differs from both sides.
func Mine(ctx context.Context, repo Repo, dir string, opts MineOptions) ([]Case, MineStats, error) {
	g := gitRepo{dir: dir}
	maxMerges := repo.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultMaxMerges
	}

	merges, err := g.merges(ctx, repo.Commit, maxMerges)
	if err != nil {
		return nil, MineStats{}, err
	}

	var stats MineStats
	var cases []Case
	for _, m := range merges {
		if opts.MaxCases > 0 && len(cases) >= opts.MaxCases {
			break
		}
		stats.Merges++
		merge, ours, theirs := m[0], m[1], m[2]

		paths, err := g.conflicts(ctx, ours, theirs)
		if err != nil {
			return nil, stats, fmt.Errorf("merge %s: %w", merge, err)
		}
		if len(paths) == 0 {
			continue
		}
		stats.ConflictingMerges++

		bases, err := g.mergeBases(ctx, ours, theirs)
		if err != nil {
			return nil, stats, fmt.Errorf("merge %s: %w", merge, err)
		}
		if len(bases) != 1 {
			stats.SkippedMergeBases++
			continue
		}
		base := bases[0]

		for _, p := range paths {
			if opts.MaxCases > 0 && len(cases) >= opts.MaxCases {
				break
			}
			stats.ConflictingFiles++
			family := familyOf(p)
			if family == "" {
				stats.OtherFamily++
				continue
			}

			c := Case{
				ID: caseID(repo.Name, merge, p), Repo: repo.Name, License: repo.License, Family: family,
				Merge: merge, Ours: ours, Theirs: theirs, Base: base, Path: p,
			}
			for _, side := range []struct {
				rev string
				dst **entry
			}{{base, &c.BaseBlob}, {ours, &c.OursBlob}, {theirs, &c.TheirsBlob}, {merge, &c.HumanBlob}} {
				e, err := g.entryAt(ctx, side.rev, p)
				if err != nil {
					return nil, stats, fmt.Errorf("merge %s, %s: %w", merge, p, err)
				}
				*side.dst = e
			}
			if c.OursBlob == nil && c.TheirsBlob == nil {
				continue
			}
			if sameEntry(c.HumanBlob, c.OursBlob) || sameEntry(c.HumanBlob, c.TheirsBlob) {
				stats.OneSide++
				continue
			}
			c.Kind = kindOf(c)

			stats.Kept++
			cases = append(cases, c)
		}
	}
	return cases, stats, nil
}

func sameEntry(a, b *entry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.OID == b.OID && a.Mode == b.Mode
}

// kindOf returns the protocol kind of the conflict of a case, as the server computes it
// (landConflictKind in internal/gitaly/service/operations/land_resolve.go).
func kindOf(c Case) string {
	switch {
	case c.OursBlob == nil || c.TheirsBlob == nil:
		return protocol.KindModifyDelete
	case c.BaseBlob == nil:
		return protocol.KindAddAdd
	case c.OursBlob.OID == c.TheirsBlob.OID:
		return protocol.KindMode
	}
	return protocol.KindContent
}
