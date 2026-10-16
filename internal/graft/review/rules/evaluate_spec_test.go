package rules

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Helpers. The generated enum/field names are those of proto/review.proto
// (ReviewVerdict.Kind, ReviewFile.Class, CHANGE_CLASS_* and KIND_*).

func specFile(path string, class gitalypb.ReviewFile_ChangeClass) *gitalypb.ReviewFile {
	return &gitalypb.ReviewFile{Path: []byte(path), Class: class}
}

func specApprove(actor string) *gitalypb.ReviewVerdict {
	return &gitalypb.ReviewVerdict{Actor: actor, Kind: gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED, Current: true}
}

func specRequestChanges(actor string) *gitalypb.ReviewVerdict {
	return &gitalypb.ReviewVerdict{Actor: actor, Kind: gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES, Current: true}
}

func specNotCurrent(v *gitalypb.ReviewVerdict) *gitalypb.ReviewVerdict {
	v.Current = false
	return v
}

// A rule applies only when a file matches both its paths and its classes;
// a rule that applies to no file yields no requirement and does not block.
func TestSpecRuleAppliesOnlyWhenPathAndClassMatch(t *testing.T) {
	rs := []config.GraftReviewRule{
		{Name: "internal-code", Paths: []string{"internal/**"}, Classes: []string{"code"}, Approvals: 1},
	}

	for _, tc := range []struct {
		name     string
		file     *gitalypb.ReviewFile
		wantReqs int
	}{
		{"path matches, class matches", specFile("internal/a.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED), 1},
		{"path matches, class differs", specFile("internal/a_test.go", gitalypb.ReviewFile_CHANGE_CLASS_TESTS), 0},
		{"path differs, class matches", specFile("other/a.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED), 0},
		{"neither matches", specFile("docs/a.md", gitalypb.ReviewFile_CHANGE_CLASS_DOCS), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqs, approved := Evaluate(rs, []*gitalypb.ReviewFile{tc.file}, "alice", nil)
			if len(reqs) != tc.wantReqs {
				t.Fatalf("requirements: got %d, want %d", len(reqs), tc.wantReqs)
			}
			if approved != (tc.wantReqs == 0) {
				t.Fatalf("approved: got %v, want %v (no applicable rule means nothing to approve)", approved, tc.wantReqs == 0)
			}
		})
	}
}

// Empty Paths and empty Classes match every file and every class.
func TestSpecEmptyPathsAndClassesMatchEverything(t *testing.T) {
	rs := []config.GraftReviewRule{{Name: "all", Approvals: 1}}

	reqs, approved := Evaluate(rs, nil, "alice", nil)
	if len(reqs) != 0 || !approved {
		t.Fatalf("no files: got %d requirements, approved=%v; want 0, true", len(reqs), approved)
	}

	files := []*gitalypb.ReviewFile{
		specFile("docs/a.md", gitalypb.ReviewFile_CHANGE_CLASS_DOCS),
		specFile("internal/a.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED),
	}
	reqs, approved = Evaluate(rs, files, "alice", nil)
	if len(reqs) != 1 {
		t.Fatalf("requirements: got %d, want 1 (one rule applies to at least one file)", len(reqs))
	}
	if approved {
		t.Fatal("approved without any approval")
	}

	_, approved = Evaluate(rs, files, "alice", []*gitalypb.ReviewVerdict{specApprove("bob")})
	if !approved {
		t.Fatal("not approved after a non-author approval")
	}
}

// Without allow_author, the author's own approval does not count; with it, it does.
func TestSpecAuthorApprovalIgnoredUnlessAllowAuthor(t *testing.T) {
	files := []*gitalypb.ReviewFile{specFile("internal/a.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED)}
	verdicts := []*gitalypb.ReviewVerdict{specApprove("alice")}

	strict := []config.GraftReviewRule{{Name: "strict", Approvals: 1, AllowAuthor: false}}
	if _, approved := Evaluate(strict, files, "alice", verdicts); approved {
		t.Fatal("author approval counted although allow_author is false")
	}

	lenient := []config.GraftReviewRule{{Name: "lenient", Approvals: 1, AllowAuthor: true}}
	if _, approved := Evaluate(lenient, files, "alice", verdicts); !approved {
		t.Fatal("author approval did not count although allow_author is true")
	}
}

// Approvers restricts who may approve; entries are globs.
func TestSpecApproversGlobRestrictsWhoCanApprove(t *testing.T) {
	rs := []config.GraftReviewRule{
		{Name: "release", Approvals: 1, Approvers: []string{"release-*"}},
	}
	files := []*gitalypb.ReviewFile{specFile("internal/a.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED)}

	if _, approved := Evaluate(rs, files, "alice", []*gitalypb.ReviewVerdict{specApprove("bob")}); approved {
		t.Fatal("approval by actor outside Approvers counted")
	}
	if _, approved := Evaluate(rs, files, "alice", []*gitalypb.ReviewVerdict{specApprove("release-bot")}); !approved {
		t.Fatal("approval by actor matching the Approvers glob did not count")
	}
}

// Only the latest verdict of each actor counts, and non-current verdicts are ignored.
func TestSpecLatestVerdictPerActorSupersedes(t *testing.T) {
	rs := []config.GraftReviewRule{{Name: "code", Approvals: 1}}
	files := []*gitalypb.ReviewFile{specFile("internal/a.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED)}

	// Verdicts are ordered oldest first.
	_, approved := Evaluate(rs, files, "alice", []*gitalypb.ReviewVerdict{
		specApprove("bob"),
		specRequestChanges("bob"),
	})
	if approved {
		t.Fatal("approved although bob's latest verdict is REQUEST_CHANGES")
	}

	_, approved = Evaluate(rs, files, "alice", []*gitalypb.ReviewVerdict{
		specRequestChanges("bob"),
		specApprove("bob"),
	})
	if !approved {
		t.Fatal("not approved although bob's latest verdict is APPROVE")
	}

	_, approved = Evaluate(rs, files, "alice", []*gitalypb.ReviewVerdict{
		specNotCurrent(specApprove("bob")),
	})
	if approved {
		t.Fatal("a non-current approval counted")
	}
}

// A current REQUEST_CHANGES blocks approval even when the rule's approvals are met;
// a non-current one does not block.
func TestSpecCurrentRequestChangesBlocksApproval(t *testing.T) {
	rs := []config.GraftReviewRule{{Name: "code", Approvals: 1}}
	files := []*gitalypb.ReviewFile{specFile("internal/a.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED)}

	_, approved := Evaluate(rs, files, "alice", []*gitalypb.ReviewVerdict{
		specApprove("bob"),
		specRequestChanges("carol"),
	})
	if approved {
		t.Fatal("approved despite a current REQUEST_CHANGES")
	}

	_, approved = Evaluate(rs, files, "alice", []*gitalypb.ReviewVerdict{
		specNotCurrent(specRequestChanges("carol")),
		specApprove("bob"),
	})
	if !approved {
		t.Fatal("a non-current REQUEST_CHANGES blocked approval")
	}
}

// Approvals is a count of distinct non-author approvers.
func TestSpecApprovalsCountDistinctActors(t *testing.T) {
	rs := []config.GraftReviewRule{{Name: "code", Approvals: 2}}
	files := []*gitalypb.ReviewFile{specFile("internal/a.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED)}

	if _, approved := Evaluate(rs, files, "alice", []*gitalypb.ReviewVerdict{
		specApprove("bob"),
		specApprove("bob"),
	}); approved {
		t.Fatal("two verdicts by the same actor counted as two approvals")
	}

	if _, approved := Evaluate(rs, files, "alice", []*gitalypb.ReviewVerdict{
		specApprove("bob"),
		specApprove("carol"),
	}); !approved {
		t.Fatal("two distinct approvers did not satisfy approvals=2")
	}
}

// Default rules: code and config changes need one approval by a non-author;
// a change that touches none of those classes needs nothing.
func TestSpecDefaultRules(t *testing.T) {
	codeAndConfig := []*gitalypb.ReviewFile{
		specFile("internal/a.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED),
		specFile("config.toml", gitalypb.ReviewFile_CHANGE_CLASS_CONFIG),
	}

	if _, approved := Evaluate(config.DefaultGraftReviewRules, codeAndConfig, "alice", nil); approved {
		t.Fatal("code/config change approved without any verdict")
	}
	if _, approved := Evaluate(config.DefaultGraftReviewRules, codeAndConfig, "alice",
		[]*gitalypb.ReviewVerdict{specApprove("alice")}); approved {
		t.Fatal("default rules accepted the author's own approval")
	}
	if _, approved := Evaluate(config.DefaultGraftReviewRules, codeAndConfig, "alice",
		[]*gitalypb.ReviewVerdict{specApprove("bob")}); !approved {
		t.Fatal("default rules rejected a non-author approval")
	}

	docsOnly := []*gitalypb.ReviewFile{specFile("doc/graft/X.md", gitalypb.ReviewFile_CHANGE_CLASS_DOCS)}
	reqs, approved := Evaluate(config.DefaultGraftReviewRules, docsOnly, "alice", nil)
	if len(reqs) != 0 || !approved {
		t.Fatalf("docs-only change: got %d requirements, approved=%v; want 0, true", len(reqs), approved)
	}
}

// Requirements come out in rule order, and repeated calls are identical.
func TestSpecDeterministicRuleOrder(t *testing.T) {
	ruleA := config.GraftReviewRule{Name: "a", Paths: []string{"a/**"}, Approvals: 1}
	ruleB := config.GraftReviewRule{Name: "b", Paths: []string{"b/**"}, Approvals: 1}
	ruleC := config.GraftReviewRule{Name: "c", Paths: []string{"c/**"}, Approvals: 1}
	files := []*gitalypb.ReviewFile{
		specFile("a/x.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED),
		specFile("b/x.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED),
		specFile("c/x.go", gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED),
	}

	rules := []config.GraftReviewRule{ruleA, ruleB, ruleC}
	first, _ := Evaluate(rules, files, "alice", nil)
	second, _ := Evaluate(rules, files, "alice", nil)
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("requirements: got %d and %d, want 3 each", len(first), len(second))
	}
	for i := range first {
		if !proto.Equal(first[i], second[i]) {
			t.Fatalf("requirement %d differs between identical calls", i)
		}
	}

	// The first requirement must be the one for rule A, the first rule in the list.
	onlyA, _ := Evaluate([]config.GraftReviewRule{ruleA}, files, "alice", nil)
	if len(onlyA) != 1 || !proto.Equal(first[0], onlyA[0]) {
		t.Fatal("first requirement is not the requirement of the first rule")
	}

	// Reversing the rule list reverses the requirement order.
	reversed, _ := Evaluate([]config.GraftReviewRule{ruleC, ruleB, ruleA}, files, "alice", nil)
	onlyC, _ := Evaluate([]config.GraftReviewRule{ruleC}, files, "alice", nil)
	if len(reversed) != 3 || !proto.Equal(reversed[0], onlyC[0]) {
		t.Fatal("requirement order does not follow the rule order")
	}
}

// ClassName maps each change class to its config name.
func TestSpecClassNames(t *testing.T) {
	want := map[gitalypb.ReviewFile_ChangeClass]string{
		gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED: "code",
		gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY:     "comment_only",
		gitalypb.ReviewFile_CHANGE_CLASS_DOCS:             "docs",
		gitalypb.ReviewFile_CHANGE_CLASS_TESTS:            "tests",
		gitalypb.ReviewFile_CHANGE_CLASS_CONFIG:           "config",
		gitalypb.ReviewFile_CHANGE_CLASS_GENERATED:        "generated",
	}
	for class, name := range want {
		if got := ClassName(class); got != name {
			t.Errorf("ClassName(%v) = %q, want %q", class, got, name)
		}
	}
}
