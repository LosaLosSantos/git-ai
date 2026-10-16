// Package classify decides the semantic change class of one file in a review.
//
// The classifier is pure Go and deterministic. It is conservative: whenever it
// cannot be sure that a change is of a narrower class, it answers
// CHANGE_CLASS_CODE_UNSPECIFIED.
package classify

import (
	"bytes"
	"path"
	"strings"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// File classifies the change of one file. old or new is nil when the file was
// added or deleted. The checks run in order and the first match wins. The
// reason explains the decision.
func File(filePath string, oldData, newData []byte) (gitalypb.ReviewFile_ChangeClass, string) {
	if reason, ok := generatedReason(filePath, oldData, newData); ok {
		return gitalypb.ReviewFile_CHANGE_CLASS_GENERATED, reason
	}
	if reason, ok := testsReason(filePath); ok {
		return gitalypb.ReviewFile_CHANGE_CLASS_TESTS, reason
	}
	if reason, ok := docsReason(filePath); ok {
		return gitalypb.ReviewFile_CHANGE_CLASS_DOCS, reason
	}
	if reason, ok := configReason(filePath); ok {
		return gitalypb.ReviewFile_CHANGE_CLASS_CONFIG, reason
	}
	return commentOnlyClass(filePath, oldData, newData)
}

// generatedSuffixes are file name suffixes of generated code.
var generatedSuffixes = []string{".pb.go", "_pb2.py", ".min.js", "_generated.go"}

// generatedReason reports whether the file is generated, by its name or by a
// generated marker in the first 20 lines of the old version (of the new version
// when the file is added). A marker that a change adds to an existing file does
// not count: otherwise adding one would turn any logic change into a generated
// change.
func generatedReason(filePath string, oldData, newData []byte) (string, bool) {
	base := path.Base(filePath)
	for _, suffix := range generatedSuffixes {
		if strings.HasSuffix(base, suffix) {
			return "generated file name (" + suffix + ")", true
		}
	}
	if oldData != nil {
		if hasGeneratedHeader(oldData) {
			return "generated marker in the first 20 lines of the old version", true
		}
		return "", false
	}
	if newData != nil && hasGeneratedHeader(newData) {
		return "added file with a generated marker in the first 20 lines", true
	}
	return "", false
}

// hasGeneratedHeader reports whether one of the first 20 lines carries
// "@generated", or "Code generated" together with "DO NOT EDIT".
func hasGeneratedHeader(content []byte) bool {
	rest := content
	for i := 0; i < 20 && len(rest) > 0; i++ {
		line := rest
		if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
			line, rest = rest[:idx], rest[idx+1:]
		} else {
			rest = nil
		}
		if bytes.Contains(line, []byte("@generated")) ||
			(bytes.Contains(line, []byte("Code generated")) && bytes.Contains(line, []byte("DO NOT EDIT"))) {
			return true
		}
	}
	return false
}

// testSuffixes are file name suffixes of test files.
var testSuffixes = []string{
	"_test.go", "_test.py",
	".spec.js", ".spec.ts", ".spec.jsx", ".spec.tsx",
	".test.js", ".test.ts", ".test.jsx", ".test.tsx",
}

// testDirs are directory names that mark test files.
var testDirs = map[string]bool{"test": true, "tests": true, "testdata": true, "__tests__": true}

// testsReason reports whether the path is a test file. Only directory
// components count, so a file that is merely named "test" is not a test.
func testsReason(filePath string) (string, bool) {
	base := path.Base(filePath)
	for _, suffix := range testSuffixes {
		if strings.HasSuffix(base, suffix) {
			return "test file name (" + suffix + ")", true
		}
	}
	if ok, _ := path.Match("test_*.py", base); ok {
		return "test file name (test_*.py)", true
	}
	for _, dir := range dirComponents(filePath) {
		if testDirs[dir] {
			return "file under test directory " + dir, true
		}
	}
	return "", false
}

// docsReason reports whether the path is documentation. A build or dependency
// manifest that happens to use a documentation extension (CMakeLists.txt,
// requirements.txt) is never documentation, wherever it is: it changes what is
// built or installed.
func docsReason(filePath string) (string, bool) {
	if isBuildManifest(filePath) {
		return "", false
	}
	if hasDocumentationExtension(filePath) {
		return "documentation file", true
	}
	for _, dir := range dirComponents(filePath) {
		if dir == "doc" || dir == "docs" {
			return "file under documentation directory " + dir, true
		}
	}
	return "", false
}

// IsDocumentationFile reports whether the file is documentation by its own
// name: it has a documentation extension (.md, .rst, .adoc, .txt) and is not a
// build or dependency manifest. A file that File classifies as DOCS only
// because it sits under a doc or docs directory (doc/handler.go, docs/x.toml)
// is not documentation by name. Callers that let a DOCS change keep earlier
// approvals use this to keep that privilege to files whose content cannot be
// compiled, built or installed.
func IsDocumentationFile(filePath string) bool {
	return hasDocumentationExtension(filePath) && !isBuildManifest(filePath)
}

// hasDocumentationExtension reports whether the file name has a documentation
// extension.
func hasDocumentationExtension(filePath string) bool {
	switch strings.ToLower(path.Ext(path.Base(filePath))) {
	case ".md", ".rst", ".adoc", ".txt":
		return true
	}
	return false
}

// isBuildManifest reports whether the file is a build or dependency manifest
// with a documentation extension: CMakeLists.txt, pip requirement and
// constraint files (requirements.txt, requirements-dev.txt,
// dev-requirements.txt, constraints.txt), and any .txt file under a
// requirements directory.
func isBuildManifest(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	if path.Ext(base) != ".txt" {
		return false
	}
	if base == "cmakelists.txt" || strings.Contains(base, "requirements") || strings.Contains(base, "constraints") {
		return true
	}
	for _, dir := range dirComponents(filePath) {
		if strings.EqualFold(dir, "requirements") {
			return true
		}
	}
	return false
}

// configReason reports whether the path is a configuration file.
func configReason(filePath string) (string, bool) {
	base := path.Base(filePath)
	switch base {
	case ".editorconfig", ".gitignore", ".gitattributes":
		return "configuration file", true
	}
	switch strings.ToLower(path.Ext(base)) {
	case ".toml", ".yaml", ".yml", ".json", ".ini", ".cfg":
		return "configuration file", true
	}
	return "", false
}

// dirComponents returns the directory components of a slash-separated path.
func dirComponents(filePath string) []string {
	parts := strings.Split(filePath, "/")
	return parts[:len(parts)-1]
}

// commentOnlyClass decides between COMMENT_ONLY and CODE by comparing the
// token streams of both versions. Any uncertainty, an added or deleted file,
// binary content or an unsupported language gives CODE.
func commentOnlyClass(filePath string, oldData, newData []byte) (gitalypb.ReviewFile_ChangeClass, string) {
	lang := languageOf(filePath)
	if lang == langUnknown {
		return gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED,
			"no path rule, and comment-only changes are only recognised in Go, Java, Rust and Python"
	}
	if oldData == nil || newData == nil {
		return gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, "file added or deleted"
	}
	oldToks, reason, ok := tokenize(lang, oldData)
	if !ok {
		return gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, "old version is not certainly lexed: " + reason
	}
	newToks, reason, ok := tokenize(lang, newData)
	if !ok {
		return gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, "new version is not certainly lexed: " + reason
	}
	if !equalTokens(oldToks, newToks) {
		return gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, "token stream changed"
	}
	return gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY, "only comments and whitespace changed"
}
