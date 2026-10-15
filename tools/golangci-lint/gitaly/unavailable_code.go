package main

import (
	"go/ast"

	"golang.org/x/tools/go/analysis"
)

const unavailableCodeAnalyzerName = "unavailable_code"

type unavailableCodeAnalyzerSettings struct {
	IncludedFunctions []string `mapstructure:"included-functions"`
}

// newErrorWrapAnalyzer warns if Unavailable status code is used. Unavailable status code is reserved to signal server's
// unavailability. It should be used by some specific components. gRPC handlers should typically avoid this type of
// error.
// For more information:
// https://gitlab.com/gitlab-org/gitaly/-/blob/master/STYLE.md?plain=0#unavailable-code
func newUnavailableCodeAnalyzer(settings *unavailableCodeAnalyzerSettings) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name: unavailableCodeAnalyzerName,
		Doc:  `discourage the usage of Unavailable status code`,
		Run:  runUnavailableCodeAnalyzer(settings.IncludedFunctions),
	}
}

func runUnavailableCodeAnalyzer(rules []string) func(*analysis.Pass) (interface{}, error) {
	return func(pass *analysis.Pass) (interface{}, error) {
		matcher := NewMatcher(pass)
		for _, file := range pass.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if matcher.MatchFunction(call, rules) {
						pass.Report(analysis.Diagnostic{
							Pos:            call.Pos(),
							End:            call.End(),
							Message:        "please avoid using the Unavailable status code: https://gitlab.com/gitlab-org/gitaly/-/blob/master/STYLE.md?plain=0#unavailable-code",
							SuggestedFixes: nil,
						})
					}
				}
				return true
			})
		}
		return nil, nil
	}
}
