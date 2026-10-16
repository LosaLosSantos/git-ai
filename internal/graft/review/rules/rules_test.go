package rules

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	classCode        = gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED
	classCommentOnly = gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY
	classDocs        = gitalypb.ReviewFile_CHANGE_CLASS_DOCS
	classTests       = gitalypb.ReviewFile_CHANGE_CLASS_TESTS
	classGenerated   = gitalypb.ReviewFile_CHANGE_CLASS_GENERATED
	classConfig      = gitalypb.ReviewFile_CHANGE_CLASS_CONFIG
)

func file(p string, class gitalypb.ReviewFile_ChangeClass) *gitalypb.ReviewFile {
	return &gitalypb.ReviewFile{Path: []byte(p), Class: class}
}

func approve(actor string, current bool) *gitalypb.ReviewVerdict {
	return &gitalypb.ReviewVerdict{Actor: actor, Kind: gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED, Current: current}
}

func requestChanges(actor string, current bool) *gitalypb.ReviewVerdict {
	return &gitalypb.ReviewVerdict{Actor: actor, Kind: gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES, Current: current}
}

// summary is a compact view of the requirements for table comparisons.
type summary struct {
	Rule      string
	Required  int32
	Approvals int32
	Satisfied bool
	Paths     []string
}

func summarize(reqs []*gitalypb.ReviewRequirement) []summary {
	var out []summary
	for _, r := range reqs {
		var paths []string
		for _, p := range r.GetPaths() {
			paths = append(paths, string(p))
		}

		out = append(out, summary{
			Rule:      r.GetRule(),
			Required:  r.GetRequiredApprovals(),
			Approvals: r.GetApprovals(),
			Satisfied: r.GetSatisfied(),
			Paths:     paths,
		})
	}

	return out
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	defaults := config.DefaultGraftReviewRules

	testCases := []struct {
		desc     string
		rules    []config.GraftReviewRule
		files    []*gitalypb.ReviewFile
		author   string
		verdicts []*gitalypb.ReviewVerdict
		wantReqs []summary
		wantAppr bool
	}{
		{
			desc:     "no rules and no files is approved",
			author:   "alice",
			wantAppr: true,
		},
		{
			desc:     "no rules configured means no requirements",
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			wantAppr: true,
		},
		{
			desc:   "default rules need an approval for code without verdicts",
			rules:  defaults,
			files:  []*gitalypb.ReviewFile{file("main.go", classCode)},
			author: "alice",
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 0, Satisfied: false, Paths: []string{"main.go"}},
			},
			wantAppr: false,
		},
		{
			desc:     "default rules are satisfied by an approval from a non-author",
			rules:    defaults,
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("bob", true)},
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 1, Satisfied: true, Paths: []string{"main.go"}},
			},
			wantAppr: true,
		},
		{
			desc:   "default rules apply to config files",
			rules:  defaults,
			files:  []*gitalypb.ReviewFile{file("config.toml", classConfig)},
			author: "alice",
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 0, Satisfied: false, Paths: []string{"config.toml"}},
			},
			wantAppr: false,
		},
		{
			desc:  "default rules do not apply to comment-only, docs, tests or generated files",
			rules: defaults,
			files: []*gitalypb.ReviewFile{
				file("a.go", classCommentOnly),
				file("README.md", classDocs),
				file("a_test.go", classTests),
				file("a.pb.go", classGenerated),
			},
			author:   "alice",
			wantAppr: true,
		},
		{
			desc:     "author approval does not count by default",
			rules:    defaults,
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("alice", true)},
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 0, Satisfied: false, Paths: []string{"main.go"}},
			},
			wantAppr: false,
		},
		{
			desc: "author approval counts with allow_author",
			rules: []config.GraftReviewRule{
				{Name: "code", Classes: []string{"code"}, Approvals: 1, AllowAuthor: true},
			},
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("alice", true)},
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 1, Satisfied: true, Paths: []string{"main.go"}},
			},
			wantAppr: true,
		},
		{
			desc:     "rule with no matching file is not reported",
			rules:    []config.GraftReviewRule{{Name: "docs", Paths: []string{"doc/**"}, Approvals: 1}},
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			wantAppr: true,
		},
		{
			desc:  "path globs select only matching files and record them",
			rules: []config.GraftReviewRule{{Name: "internal", Paths: []string{"internal/**"}, Approvals: 1}},
			files: []*gitalypb.ReviewFile{
				file("internal/a/b.go", classCode),
				file("cmd/x.go", classCode),
				file("internal/c.go", classCode),
			},
			author: "alice",
			wantReqs: []summary{
				{Rule: "internal", Required: 1, Approvals: 0, Satisfied: false, Paths: []string{"internal/a/b.go", "internal/c.go"}},
			},
			wantAppr: false,
		},
		{
			desc:     "class filter excludes files of other classes",
			rules:    []config.GraftReviewRule{{Name: "code", Classes: []string{"code"}, Approvals: 1}},
			files:    []*gitalypb.ReviewFile{file("a.toml", classConfig)},
			author:   "alice",
			wantAppr: true,
		},
		{
			desc: "rule needs every rule that applies to be satisfied",
			rules: []config.GraftReviewRule{
				{Name: "code", Classes: []string{"code"}, Approvals: 1},
				{Name: "config", Classes: []string{"config"}, Approvals: 2},
			},
			files:    []*gitalypb.ReviewFile{file("main.go", classCode), file("c.yaml", classConfig)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("bob", true)},
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 1, Satisfied: true, Paths: []string{"main.go"}},
				{Rule: "config", Required: 2, Approvals: 1, Satisfied: false, Paths: []string{"c.yaml"}},
			},
			wantAppr: false,
		},
		{
			desc: "requirements follow rule order",
			rules: []config.GraftReviewRule{
				{Name: "all-files", Approvals: 0},
				{Name: "config-only", Classes: []string{"config"}, Approvals: 0},
				{Name: "code-only", Classes: []string{"code"}, Approvals: 0},
			},
			files:  []*gitalypb.ReviewFile{file("main.go", classCode), file("c.toml", classConfig)},
			author: "alice",
			wantReqs: []summary{
				{Rule: "all-files", Required: 0, Approvals: 0, Satisfied: true, Paths: []string{"main.go", "c.toml"}},
				{Rule: "config-only", Required: 0, Approvals: 0, Satisfied: true, Paths: []string{"c.toml"}},
				{Rule: "code-only", Required: 0, Approvals: 0, Satisfied: true, Paths: []string{"main.go"}},
			},
			wantAppr: true,
		},
		{
			desc:     "approver globs restrict who counts",
			rules:    []config.GraftReviewRule{{Name: "bots", Approvers: []string{"bot-*"}, Approvals: 1}},
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("carol", true)},
			wantReqs: []summary{
				{Rule: "bots", Required: 1, Approvals: 0, Satisfied: false, Paths: []string{"main.go"}},
			},
			wantAppr: false,
		},
		{
			desc:     "approver glob match counts",
			rules:    []config.GraftReviewRule{{Name: "bots", Approvers: []string{"bot-*"}, Approvals: 1}},
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("bot-1", true)},
			wantReqs: []summary{
				{Rule: "bots", Required: 1, Approvals: 1, Satisfied: true, Paths: []string{"main.go"}},
			},
			wantAppr: true,
		},
		{
			desc:     "a malformed approver glob matches nobody",
			rules:    []config.GraftReviewRule{{Name: "bots", Approvers: []string{"["}, Approvals: 1}},
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("[", true)},
			wantReqs: []summary{
				{Rule: "bots", Required: 1, Approvals: 0, Satisfied: false, Paths: []string{"main.go"}},
			},
			wantAppr: false,
		},
		{
			desc:   "a malformed path glob makes the rule apply",
			rules:  []config.GraftReviewRule{{Name: "broken", Paths: []string{"["}, Approvals: 1}},
			files:  []*gitalypb.ReviewFile{file("main.go", classCode)},
			author: "alice",
			wantReqs: []summary{
				{Rule: "broken", Required: 1, Approvals: 0, Satisfied: false, Paths: []string{"main.go"}},
			},
			wantAppr: false,
		},
		{
			desc:     "latest verdict of an actor wins: approve then request changes",
			rules:    defaults,
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("bob", true), requestChanges("bob", true)},
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 0, Satisfied: false, Paths: []string{"main.go"}},
			},
			wantAppr: false,
		},
		{
			desc:     "latest verdict of an actor wins: request changes then approve",
			rules:    defaults,
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{requestChanges("bob", true), approve("bob", true)},
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 1, Satisfied: true, Paths: []string{"main.go"}},
			},
			wantAppr: true,
		},
		{
			desc:     "one actor approving twice counts once",
			rules:    []config.GraftReviewRule{{Name: "code", Approvals: 2}},
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("bob", true), approve("bob", true)},
			wantReqs: []summary{
				{Rule: "code", Required: 2, Approvals: 1, Satisfied: false, Paths: []string{"main.go"}},
			},
			wantAppr: false,
		},
		{
			desc:     "non-current verdicts do not count",
			rules:    defaults,
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("bob", false)},
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 0, Satisfied: false, Paths: []string{"main.go"}},
			},
			wantAppr: false,
		},
		{
			desc:     "a non-current request for changes does not block",
			rules:    defaults,
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("bob", true), requestChanges("carol", false)},
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 1, Satisfied: true, Paths: []string{"main.go"}},
			},
			wantAppr: true,
		},
		{
			// Regression: the latest verdict is chosen before currency is
			// checked, so a superseded approval that is still current does not
			// count again when the later request for changes became non-current.
			desc:     "a current approval superseded by a non-current request for changes does not count",
			rules:    defaults,
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("bob", true), requestChanges("bob", false)},
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 0, Satisfied: false, Paths: []string{"main.go"}},
			},
			wantAppr: false,
		},
		{
			desc:     "a current request for changes superseded by a non-current approval does not block",
			rules:    []config.GraftReviewRule{{Name: "code", Approvals: 1}},
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{requestChanges("bob", true), approve("bob", false), approve("carol", true)},
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 1, Satisfied: true, Paths: []string{"main.go"}},
			},
			wantAppr: true,
		},
		{
			desc:     "a current request for changes blocks even when approvals are satisfied",
			rules:    defaults,
			files:    []*gitalypb.ReviewFile{file("main.go", classCode)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{approve("bob", true), requestChanges("carol", true)},
			wantReqs: []summary{
				{Rule: "code", Required: 1, Approvals: 1, Satisfied: true, Paths: []string{"main.go"}},
			},
			wantAppr: false,
		},
		{
			desc:     "a request for changes blocks a review with no rules applying",
			rules:    defaults,
			files:    []*gitalypb.ReviewFile{file("README.md", classDocs)},
			author:   "alice",
			verdicts: []*gitalypb.ReviewVerdict{requestChanges("bob", true)},
			wantAppr: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			reqs, approved := Evaluate(tc.rules, tc.files, tc.author, tc.verdicts)
			require.Equal(t, tc.wantAppr, approved)
			require.Equal(t, tc.wantReqs, summarize(reqs))
		})
	}
}

func TestEvaluateDeterministic(t *testing.T) {
	t.Parallel()

	rules := []config.GraftReviewRule{
		{Name: "a", Classes: []string{"code"}, Approvals: 2, Approvers: []string{"*"}},
		{Name: "b", Paths: []string{"x/**"}, Approvals: 1, AllowAuthor: true},
		{Name: "c", Approvals: 1, Approvers: []string{"bob", "carol"}},
	}
	files := []*gitalypb.ReviewFile{
		file("x/1.go", classCode),
		file("x/2.toml", classConfig),
		file("y.go", classCode),
	}
	verdicts := []*gitalypb.ReviewVerdict{
		approve("bob", true),
		approve("carol", true),
		approve("dave", true),
		requestChanges("erin", false),
	}

	firstReqs, firstApproved := Evaluate(rules, files, "alice", verdicts)
	for i := 0; i < 50; i++ {
		reqs, approved := Evaluate(rules, files, "alice", verdicts)
		require.Equal(t, firstApproved, approved)
		require.Len(t, reqs, len(firstReqs))
		for j := range reqs {
			require.True(t, proto.Equal(firstReqs[j], reqs[j]))
		}
	}
}

func TestClassName(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		class gitalypb.ReviewFile_ChangeClass
		want  string
	}{
		{classCode, "code"},
		{classCommentOnly, "comment_only"},
		{classDocs, "docs"},
		{classTests, "tests"},
		{classGenerated, "generated"},
		{classConfig, "config"},
		{gitalypb.ReviewFile_ChangeClass(99), "code"},
	}

	for _, tc := range testCases {
		require.Equal(t, tc.want, ClassName(tc.class), "class %v", tc.class)
	}
}

func TestMatchGlob(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		glob, s string
		want    bool
	}{
		{"**", "anything/at/all", true},
		{"**", "", true},
		{"*", "main.go", true},
		{"*", "dir/main.go", false},
		{"internal/**", "internal/a/b.go", true},
		{"internal/**", "internal/a.go", true},
		{"internal/**", "internal", false},
		{"internal/**", "internalx/a.go", false},
		{"refs/heads/**", "refs/heads/main", true},
		{"refs/heads/**", "refs/heads/a/b", true},
		{"refs/heads/**", "refs/heads", false},
		{"refs/heads/release**", "refs/heads/release/1.0", true},
		{"refs/heads/release**", "refs/heads/release-2", true},
		{"refs/heads/release**", "refs/heads/main", false},
		{"bot-*", "bot-1", true},
		{"bot-*", "alice", false},
		{"a?c", "abc", true},
		{"[", "abc", false},
	}

	for _, tc := range testCases {
		got, err := matchGlob(tc.glob, tc.s)
		if tc.glob == "[" {
			require.Error(t, err)
			continue
		}

		require.NoError(t, err, "glob %q", tc.glob)
		require.Equal(t, tc.want, got, "glob %q against %q", tc.glob, tc.s)
	}
}
