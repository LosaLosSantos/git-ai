package rules

import (
	"path"
	"strings"
)

// matchGlob matches s against a glob with the semantics of the standalone
// policy (internal/gitlab, matchRef): path.Match syntax, a trailing "/**"
// matches the prefix at any depth, any other trailing "**" matches any
// remainder including "/", and a bare "**" matches everything. The matcher is
// copied rather than imported so that this package stays free of the
// standalone client.
func matchGlob(glob, s string) (bool, error) {
	if glob == "**" {
		return true, nil
	}

	prefix, deep := strings.CutSuffix(glob, "/**")
	if !deep {
		if loose, ok := strings.CutSuffix(glob, "**"); ok {
			return matchPrefix(loose, s)
		}

		return path.Match(glob, s)
	}

	// Validate the pattern up front: path.Match only reports a malformed
	// pattern when it gets to examine it against a candidate.
	if _, err := path.Match(prefix, ""); err != nil {
		return false, err
	}

	// Every "/" in s ends a candidate directory prefix of s. Any of them
	// matching the glob prefix means s lies at or below that directory.
	for i := 0; i < len(s); i++ {
		if s[i] != '/' {
			continue
		}

		ok, err := path.Match(prefix, s[:i])
		if err != nil {
			return false, err
		}

		if ok {
			return true, nil
		}
	}

	return false, nil
}

// matchPrefix reports whether some prefix of s (possibly s itself) matches
// the path.Match pattern prefix, so that the rest of s is matched by a
// trailing "**".
func matchPrefix(prefix, s string) (bool, error) {
	if _, err := path.Match(prefix, ""); err != nil {
		return false, err
	}

	for i := len(s); i >= 0; i-- {
		ok, err := path.Match(prefix, s[:i])
		if err != nil {
			return false, err
		}

		if ok {
			return true, nil
		}
	}

	return false, nil
}
