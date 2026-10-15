package main

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"

	"golang.org/x/tools/go/analysis"
)

const quoteInterpolationAnalyzerName = "string_interpolation_quote"

type quoteInterpolationAnalyzerSettings struct {
	IncludedFunctions []string `mapstructure:"included-functions"`
}

// newQuoteInterpolationAnalyzer returns an analyzer to detect manually quoted string interpolation
// with '%s' and "%s". Quoting this way doesn't escape special characters such as endline and makes
// debugging harder later. We encourage to use %q verb instead.
//
//   - Bad
//     return fmt.Errorf("gl_id='%s' is invalid", glID)
//
//   - Bad
//     fmt.Sprintf("fatal: not a git repository: '%s'", repoPath)
//
//   - Good
//     return fmt.Errorf("gl_id=%q is invalid", glID)
//
//   - Good
//     fmt.Sprintf("fatal: not a git repository: %q", repoPath)
//
// For more information:
// https://gitlab.com/gitlab-org/gitaly/-/blob/master/STYLE.md#use-q-when-interpolating-strings
func newQuoteInterpolationAnalyzer(settings *quoteInterpolationAnalyzerSettings) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name: quoteInterpolationAnalyzerName,
		Doc: `Unless it would lead to incorrect results, always use %q when
		interpolating strings. For more information:
		https://gitlab.com/gitlab-org/gitaly/-/blob/master/STYLE.md#use-q-when-interpolating-strings`,
		Run: runStringInterpolationQuoteAnalyzer(settings.IncludedFunctions),
	}
}

// offendedFormatPattern matches string interpolation having '%s' and "%s" format
var offendedFormatPattern = regexp.MustCompile(`['"]%s['"]`)

func analyzeInterpolation(call *ast.CallExpr, pass *analysis.Pass) {
	if len(call.Args) <= 1 {
		return
	}

	if str, ok := call.Args[0].(*ast.BasicLit); ok && str.Kind == token.STRING {
		value := str.Value
		if strings.HasPrefix(value, `'`) || strings.HasPrefix(value, `"`) {
			value = value[1:]
		}
		if strings.HasSuffix(value, `'`) || strings.HasSuffix(value, `"`) {
			value = value[:len(value)-1]
		}
		for _, index := range offendedFormatPattern.FindAllStringIndex(value, -1) {
			start := token.Pos(int(str.Pos()) + index[0] + 1)
			end := token.Pos(int(str.Pos()) + index[1])
			pass.Report(analysis.Diagnostic{
				Pos:            start,
				End:            end,
				Message:        "wrapping %s verb with quotes is not encouraged, please use %q instead",
				SuggestedFixes: nil,
			})
		}
	}
}

func runStringInterpolationQuoteAnalyzer(rules []string) func(*analysis.Pass) (interface{}, error) {
	return func(pass *analysis.Pass) (interface{}, error) {
		matcher := NewMatcher(pass)
		for _, file := range pass.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if matcher.MatchFunction(call, rules) {
						analyzeInterpolation(call, pass)
					}
				}
				return true
			})
		}
		return nil, nil
	}
}
