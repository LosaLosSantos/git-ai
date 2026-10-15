package main

import (
	"go/ast"
	"go/types"
	"regexp"

	"golang.org/x/tools/go/analysis"
)

// Matcher implements some helper methods to filter relevant AST nodes for linter checks. It depends
// on the TypeInfo of analysis.Pass object passed in the analyzer.
type Matcher struct {
	typesInfo *types.Info
}

// NewMatcher creates a new Matcher object from the input analysis pass.
func NewMatcher(pass *analysis.Pass) *Matcher {
	return &Matcher{
		typesInfo: pass.TypesInfo,
	}
}

var funcNamePattern = regexp.MustCompile(`^\(?([^\\)].*)\)?\.(.*)$`)

// MatchFunction returns true if the input call expression matches any of the list of input rules.
// A rule is a human-friend full name of a function. Some examples:
//   - A public package function:
//     "fmt.Errorf"
//   - Match all package functions:
//     "fmt.*"
//   - A public function of a dependent package:
//     "gitlab.com/gitlab-org/gitaly/v15/internal/structerr.NewInternal",
//   - A function of a struct inside a package:
//     "(*gitlab.com/gitlab-org/gitaly/v15/internal/structerr.Error).Unwrap",
//   - A local function call:
//     "New(1)",
//
// This Matcher doesn't support interface match (yet).
func (m *Matcher) MatchFunction(call *ast.CallExpr, rules []string) bool {
	name := m.functionName(call)
	if name == "" {
		return false
	}
	for _, rule := range rules {
		if m.matchRule(name, rule) {
			return true
		}
	}
	return false
}

func (m *Matcher) matchRule(name, rule string) bool {
	nameMatches := funcNamePattern.FindStringSubmatch(name)
	if len(nameMatches) == 0 {
		return false
	}

	ruleMatches := funcNamePattern.FindStringSubmatch(rule)
	if len(ruleMatches) == 0 {
		return false
	}

	if nameMatches[1] != ruleMatches[1] {
		return false
	}

	return ruleMatches[2] == "*" || nameMatches[2] == ruleMatches[2]
}

func (m *Matcher) functionName(call *ast.CallExpr) string {
	fn, ok := m.getFunction(call)
	if !ok {
		return ""
	}

	return fn.FullName()
}

func (m *Matcher) getFunction(call *ast.CallExpr) (*types.Func, bool) {
	var ident *ast.Ident

	switch ty := call.Fun.(type) {
	case *ast.SelectorExpr:
		ident = ty.Sel
	case *ast.Ident:
		ident = ty
	default:
		return nil, false
	}

	fn, ok := m.typesInfo.ObjectOf(ident).(*types.Func)
	if !ok {
		return nil, false
	}
	return fn, true
}
