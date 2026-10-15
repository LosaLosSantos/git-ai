package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"regexp"

	"golang.org/x/tools/go/analysis"
)

const errorWrapAnalyzerName = "error_wrap"

type errorWrapAnalyzerSettings struct {
	IncludedFunctions []string `mapstructure:"included-functions"`
}

// newErrorWrapAnalyzer returns an analyzer to detect unexpected error interpolation without %w.
// After error wrapping was introduced, we encourage wrapping error with %w when constructing a new
// error. The new error contains the original error able to be unwrapped later.
//
//   - Bad
//     return structerr.NewInvalidArgument("GetRepoPath: %s", err)
//
//   - Bad
//     return structerr.NewCanceled("%v", err)
//
//   - Bad
//     return nil, fmt.Errorf("unmarshalling json: %v", err)
//
//   - Good
//     return structerr.NewCanceled("%w", err)
//
//   - Good
//     return nil, fmt.Errorf("failed unmarshalling json: %w", err)
//
// For more information:
// https://gitlab.com/gitlab-org/gitaly/-/blob/master/STYLE.md#use-w-when-wrapping-errors
func newErrorWrapAnalyzer(settings *errorWrapAnalyzerSettings) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name: errorWrapAnalyzerName,
		Doc: `Always wrap an error with %w:
	https://gitlab.com/gitlab-org/gitaly/-/blob/master/STYLE.md#use-w-when-wrapping-errors`,
		Run: runErrorWrapAnalyzer(settings.IncludedFunctions),
	}
}

var errorType = mustFindBuiltinInterface("error")

// Over-simplified pattern to parse interpolation format. This linter targets error wrapping only.
// Most of the time, %s or %v or %q are used. This pattern is good enough to detect most cases.
// The std uses a proper parser, which is overkilled for re-implemented:
// https://github.com/golang/go/blob/518889b35cb07f3e71963f2ccfc0f96ee26a51ce/src/fmt/print.go#L1026
var formatPattern = regexp.MustCompile(`%.`)

func analyzeErrorInterpolation(pass *analysis.Pass, call *ast.CallExpr) {
	if len(call.Args) <= 1 {
		// Irrelevant call, or static format
		return
	}

	if str, ok := call.Args[0].(*ast.BasicLit); ok && str.Kind == token.STRING {
		verbs := formatPattern.FindAllStringIndex(str.Value, -1)

		if len(verbs) != len(call.Args)-1 {
			// Mismatched format verbs and arguments; or our regexp is not correct
			return
		}
		for index, arg := range call.Args[1:] {
			argType := pass.TypesInfo.Types[arg].Type
			if types.Implements(argType, errorType) {
				verb := str.Value[verbs[index][0]:verbs[index][1]]
				if verb != "%w" {
					pass.Report(analysis.Diagnostic{
						Pos:            token.Pos(int(str.Pos()) + verbs[index][0]),
						End:            token.Pos(int(str.Pos()) + verbs[index][1]),
						Message:        "please use %w to wrap errors",
						SuggestedFixes: nil,
					})
				}
			}
		}
	}
}

func runErrorWrapAnalyzer(rules []string) func(*analysis.Pass) (interface{}, error) {
	return func(pass *analysis.Pass) (interface{}, error) {
		matcher := NewMatcher(pass)
		for _, file := range pass.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if matcher.MatchFunction(call, rules) {
						analyzeErrorInterpolation(pass, call)
					}
				}
				return true
			})
		}
		return nil, nil
	}
}
