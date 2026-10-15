package resolver

import (
	"path"
	"strings"
)

// Allowed reports whether the conflicted path p may be sent to the resolver.
// Globs have the same syntax as policy refs: path.Match patterns, where a
// trailing "/**" matches the prefix at any depth ("secrets/**" matches
// "secrets/a/b.env" but not "secrets") and any other trailing "**" matches any
// remainder. A bare "**" matches every path.
//
// Deny wins over allow. An empty allow list allows every path that is not
// denied. Paths that are not clean relative paths are never allowed. The
// policy fails closed: a malformed deny glob denies, a malformed allow glob
// matches nothing.
func Allowed(p string, allow, deny []string) bool {
	if !cleanRelPath(p) {
		return false
	}

	for _, glob := range deny {
		matched, err := matchGlob(glob, p)
		if err != nil || matched {
			return false
		}
	}

	if len(allow) == 0 {
		return true
	}

	for _, glob := range allow {
		if matched, err := matchGlob(glob, p); err == nil && matched {
			return true
		}
	}

	return false
}

// ActorAllowed reports whether actor may request resolution given the actor
// globs. An empty list allows every caller. Otherwise the actor must be
// non-empty and match one glob; a malformed glob does not match.
func ActorAllowed(actor string, globs []string) bool {
	if len(globs) == 0 {
		return true
	}

	if actor == "" {
		return false
	}

	for _, glob := range globs {
		if matched, err := matchGlob(glob, actor); err == nil && matched {
			return true
		}
	}

	return false
}

// PathAllowed reports whether the client may send the conflicted path p to
// the resolver. A nil client allows nothing.
func (c *Client) PathAllowed(p string) bool {
	if c == nil {
		return false
	}

	return Allowed(p, c.pathsAllow, c.pathsDeny)
}

// ActorAllowed reports whether the client may resolve for actor. A nil client
// allows nothing.
func (c *Client) ActorAllowed(actor string) bool {
	if c == nil {
		return false
	}

	return ActorAllowed(actor, c.actors)
}

// matchGlob matches s against a policy glob, see Allowed. The semantics mirror
// the ref globs of the standalone policy (internal/gitlab), which cannot be
// imported from here.
func matchGlob(glob, s string) (bool, error) {
	if glob == "**" {
		return true, nil
	}

	prefix, deep := strings.CutSuffix(glob, "/**")
	if !deep {
		if loose, ok := strings.CutSuffix(glob, "**"); ok {
			return matchGlobPrefix(loose, s)
		}

		return path.Match(glob, s)
	}

	// path.Match only reports a malformed pattern when it examines a
	// candidate, so validate the pattern up front.
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

// matchGlobPrefix reports whether some prefix of s (possibly s itself) matches
// the pattern, so that the remainder is covered by a trailing "**".
func matchGlobPrefix(pattern, s string) (bool, error) {
	if _, err := path.Match(pattern, ""); err != nil {
		return false, err
	}

	for i := len(s); i >= 0; i-- {
		ok, err := path.Match(pattern, s[:i])
		if err != nil {
			return false, err
		}

		if ok {
			return true, nil
		}
	}

	return false, nil
}

// cleanRelPath reports whether p is a clean relative slash path with no empty,
// ".", ".." or absolute components and no NUL byte.
func cleanRelPath(p string) bool {
	if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") {
		return false
	}

	if strings.ContainsRune(p, 0) {
		return false
	}

	return path.Clean(p) == p
}
