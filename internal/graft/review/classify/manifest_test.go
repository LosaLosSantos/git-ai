package classify

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression: build and dependency manifests with a .txt extension change what
// is built or installed, so they are never DOCS, wherever they are.
func TestFileBuildManifestsAreNotDocs(t *testing.T) {
	runFileCases(t, []fileCase{
		{"requirements", "requirements.txt", "requests==2.0\n", "requests==2.0\nevil==1.0\n", clsCode},
		{"requirements variant", "requirements-dev.txt", "a\n", "b\n", clsCode},
		{"dev requirements", "py/dev-requirements.txt", "a\n", "b\n", clsCode},
		{"constraints", "constraints.txt", "a\n", "b\n", clsCode},
		{"requirements directory", "requirements/base.txt", "a\n", "b\n", clsCode},
		{"cmake lists", "CMakeLists.txt", "add_executable(a a.c)\n", "add_executable(a a.c b.c)\n", clsCode},
		{"cmake lists under docs", "docs/CMakeLists.txt", "a\n", "b\n", clsCode},
		{"requirements under docs", "docs/requirements.txt", "a\n", "b\n", clsCode},
		{"added requirements", "requirements.txt", absentSentinel, "evil==1.0\n", clsCode},
		// Ordinary text files keep their documentation class.
		{"plain text file", "NOTES.txt", "one\n", "two\n", clsDocs},
		{"license text", "LICENSE.txt", "one\n", "two\n", clsDocs},
	})
}

func TestIsDocumentationFile(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"README.md", true},
		{"doc/guide.rst", true},
		{"docs/x.adoc", true},
		{"NOTES.txt", true},
		// DOCS by directory only, not by name.
		{"doc/x.go", false},
		{"internal/doc/handler.go", false},
		{"docs/x.toml", false},
		{"docs/Makefile", false},
		// Manifests.
		{"requirements.txt", false},
		{"CMakeLists.txt", false},
		{"docs/requirements.txt", false},
		{"main.go", false},
	} {
		require.Equal(t, tc.want, IsDocumentationFile(tc.path), "IsDocumentationFile(%q)", tc.path)
	}
}
