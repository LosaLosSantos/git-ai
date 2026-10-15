package superfan

import "testing"

func TestMatchPath(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"internal/pay/**", "internal/pay/x.go", true},
		{"internal/pay/**", "internal/pay/a/b/c.go", true},
		{"internal/pay/**", "internal/pay", true},
		{"internal/pay/**", "internal/payroll/x.go", false},
		{"internal/pay/**", "internal/x.go", false},
		{"**", "any/where/file", true},
		{"**/go.mod", "go.mod", true},
		{"**/go.mod", "svc/a/go.mod", true},
		{"**/go.mod", "svc/a/go.sum", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "docs/a/b.md", false},
		{"a/?/c", "a/b/c", true},
		{"a/?/c", "a/bb/c", false},
		{"a/[bc]/d", "a/c/d", true},
		{"a/[bc]/d", "a/x/d", false},
		{"CHANGELOG.md", "CHANGELOG.md", true},
		{"CHANGELOG.md", "changelog.md", false},
		{"a/**/b/**", "a/x/y/b/z/w", true},
		{"a/**/b/**", "a/x/y/c", false},
		{"[", "[", false}, // malformed patterns match nothing
		{"", "x", false},
		{"/abs/**", "/abs/x", false},
		{"../x/**", "../x/y", false},
	} {
		if got := MatchPath(tc.pattern, tc.name); got != tc.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestValidGlob(t *testing.T) {
	long := make([]byte, maxGlobLength+1)
	for i := range long {
		long[i] = 'a'
	}
	for _, tc := range []struct {
		pattern string
		ok      bool
	}{
		{"internal/**", true},
		{"**", true},
		{"", false},
		{"/abs", false},
		{"trailing/", false},
		{"a//b", false},
		{"a/../b", false},
		{"a/[/b", false},
		{string(long), false},
		{"**/**/**/**", true},
		{"**/**/**/**/**", false},
	} {
		err := ValidGlob(tc.pattern)
		if (err == nil) != tc.ok {
			t.Errorf("ValidGlob(%q) = %v, want ok=%v", tc.pattern, err, tc.ok)
		}
	}
}

func TestValidPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"a/b/c.go", true},
		{"", false},
		{".", false},
		{"/etc/passwd", false},
		{"a/../b", false},
		{"../b", false},
		{"a//b", false},
		{"a/./b", false},
		{"a/", false},
		{`a\b`, false},
		{"a\x00b", false},
		{"a\nb", false},
	} {
		err := ValidPath(tc.path)
		if (err == nil) != tc.ok {
			t.Errorf("ValidPath(%q) = %v, want ok=%v", tc.path, err, tc.ok)
		}
	}
}
