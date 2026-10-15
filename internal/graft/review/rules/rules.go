// Package rules evaluates the approval rules of a review. It is pure Go: the
// caller supplies the rules, the classified files and the verdicts, and gets
// back the requirements and whether the review is approved. See
// doc/graft/PLAN5.md (WP45).
package rules

import (
	"fmt"
	"strings"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// Evaluate returns one requirement per rule that applies to at least one
// file, in rule order, and whether the review is approved: every such
// requirement is satisfied and there is no current REQUEST_CHANGES verdict.
//
// A rule applies to a file when the file's path matches one of rule.Paths (or
// rule.Paths is empty) and the file's class name is in rule.Classes (or
// rule.Classes is empty). Only the latest verdict of each actor counts (the
// verdicts are ordered oldest first), and only when it is current: an earlier
// current verdict never stands in for a later one that is not. An approval
// counts for a rule when its actor matches one of rule.Approvers (or
// rule.Approvers is empty) and is not the author unless rule.AllowAuthor is
// set. Callers pass the configured rules, for example
// cfg.Review.RulesOrDefault(), so no defaults are applied here.
func Evaluate(rules []config.GraftReviewRule, files []*gitalypb.ReviewFile, author string, verdicts []*gitalypb.ReviewVerdict) ([]*gitalypb.ReviewRequirement, bool) {
	latest := latestCurrentVerdicts(verdicts)

	approved := true
	for _, verdict := range latest {
		if verdict.GetKind() == gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES {
			approved = false
		}
	}

	var requirements []*gitalypb.ReviewRequirement
	for _, rule := range rules {
		var applied [][]byte
		for _, file := range files {
			if ruleApplies(rule, file) {
				applied = append(applied, file.GetPath())
			}
		}

		if len(applied) == 0 {
			continue
		}

		approvals := countApprovals(rule, latest, author)
		satisfied := approvals >= rule.Approvals
		if !satisfied {
			approved = false
		}

		requirements = append(requirements, &gitalypb.ReviewRequirement{
			Rule:              rule.Name,
			Description:       describe(rule),
			RequiredApprovals: int32(rule.Approvals),
			Approvals:         int32(approvals),
			Satisfied:         satisfied,
			Paths:             applied,
		})
	}

	return requirements, approved
}

// ClassName maps a ChangeClass to its configuration name: "code",
// "comment_only", "docs", "tests", "generated" or "config". The unspecified
// class and any unknown value map to "code", the most demanding class.
func ClassName(c gitalypb.ReviewFile_ChangeClass) string {
	switch c {
	case gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY:
		return "comment_only"
	case gitalypb.ReviewFile_CHANGE_CLASS_DOCS:
		return "docs"
	case gitalypb.ReviewFile_CHANGE_CLASS_TESTS:
		return "tests"
	case gitalypb.ReviewFile_CHANGE_CLASS_GENERATED:
		return "generated"
	case gitalypb.ReviewFile_CHANGE_CLASS_CONFIG:
		return "config"
	default:
		return "code"
	}
}

// latestCurrentVerdicts returns the latest verdict of each actor, keeping it
// only when it is current. The latest verdict is chosen among all verdicts
// before currency is checked: an actor whose latest verdict is not current has
// no verdict that counts, so an earlier verdict that is still current (for
// example an approval superseded by a later request for changes) never counts
// again.
func latestCurrentVerdicts(verdicts []*gitalypb.ReviewVerdict) map[string]*gitalypb.ReviewVerdict {
	latest := make(map[string]*gitalypb.ReviewVerdict)
	for _, verdict := range verdicts {
		latest[verdict.GetActor()] = verdict
	}

	for actor, verdict := range latest {
		if !verdict.GetCurrent() {
			delete(latest, actor)
		}
	}

	return latest
}

// ruleApplies reports whether rule governs the file.
func ruleApplies(rule config.GraftReviewRule, file *gitalypb.ReviewFile) bool {
	if len(rule.Classes) > 0 && !contains(rule.Classes, ClassName(file.GetClass())) {
		return false
	}

	if len(rule.Paths) == 0 {
		return true
	}

	for _, glob := range rule.Paths {
		ok, err := matchGlob(glob, string(file.GetPath()))
		if err != nil {
			// A malformed glob is rejected when the configuration is loaded.
			// If one gets here anyway, the rule applies: the file is never
			// left unchecked by mistake.
			return true
		}

		if ok {
			return true
		}
	}

	return false
}

// countApprovals counts the distinct actors whose latest current verdict is an
// approval and that may approve for rule.
func countApprovals(rule config.GraftReviewRule, latest map[string]*gitalypb.ReviewVerdict, author string) int {
	approvals := 0
	for actor, verdict := range latest {
		if verdict.GetKind() != gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED {
			continue
		}

		if actor == author && !rule.AllowAuthor {
			continue
		}

		if !approverMatches(rule.Approvers, actor) {
			continue
		}

		approvals++
	}

	return approvals
}

// approverMatches reports whether actor matches one of the approver globs, or
// whether there are no globs. A malformed glob matches nobody.
func approverMatches(globs []string, actor string) bool {
	if len(globs) == 0 {
		return true
	}

	for _, glob := range globs {
		ok, err := matchGlob(glob, actor)
		if err == nil && ok {
			return true
		}
	}

	return false
}

// describe explains what rule requires, for requirements shown to people.
func describe(rule config.GraftReviewRule) string {
	who := "any identity"
	if len(rule.Approvers) > 0 {
		who = "identities matching " + strings.Join(rule.Approvers, ", ")
	}

	author := "the author's approval does not count"
	if rule.AllowAuthor {
		author = "the author's approval counts"
	}

	return fmt.Sprintf("%d approval(s) from %s; %s", rule.Approvals, who, author)
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}

	return false
}
