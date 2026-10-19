package git

import (
	"bytes"
	"fmt"
)

type validateRevisionConfig struct {
	allowEmpty              bool
	allowPathScopedRevision bool
	allowPseudoRevisions    bool
	allowLineBreaksInPath   bool
}

// ValidateRevisionOption is an option that can be passed to ValidateRevision.
type ValidateRevisionOption func(cfg *validateRevisionConfig)

// AllowEmptyRevision changes ValidateRevision to not return an error in case the specified
// revision is empty.
func AllowEmptyRevision() ValidateRevisionOption {
	return func(cfg *validateRevisionConfig) {
		cfg.allowEmpty = true
	}
}

// AllowPathScopedRevision changes ValidateRevision to allow path-scoped revisions like
// `HEAD:README.md`. The path of a path-scoped revision may contain any character except for NUL
// bytes and, unless AllowLineBreaksInPath is given, line breaks: revisions travel to git one per
// line on its standard input (git-rev-list(1) --stdin), so a line break in a revision would be
// another revision, and NUL ends the line for git.
func AllowPathScopedRevision() ValidateRevisionOption {
	return func(cfg *validateRevisionConfig) {
		cfg.allowPathScopedRevision = true
	}
}

// AllowLineBreaksInPath changes ValidateRevision to allow a line feed or a carriage return in the
// path of a path-scoped revision. Only a caller whose revisions git reads NUL-delimited may give
// it (git-cat-file(1) -Z): for any other, a line break ends the revision.
func AllowLineBreaksInPath() ValidateRevisionOption {
	return func(cfg *validateRevisionConfig) {
		cfg.allowLineBreaksInPath = true
	}
}

// AllowPseudoRevision changes ValidateRevision to allow pseudo-revisions understood by
// git-rev-list(1). This includes options like `--all`, `--not`, `--branches` or `--glob`.
func AllowPseudoRevision() ValidateRevisionOption {
	return func(cfg *validateRevisionConfig) {
		cfg.allowPseudoRevisions = true
	}
}

// ValidateRevision checks if a revision looks valid. The default behaviour can be changed by
// passing ValidateRevisionOptions.
func ValidateRevision(revision []byte, opts ...ValidateRevisionOption) error {
	var cfg validateRevisionConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.allowPseudoRevisions && isPseudoRevision(revision) {
		// The pattern after "=" is a reference pattern: it has no control character, and above all
		// no line break or NUL, which would make the rest of it another revision on git's stdin.
		if hasControlCharacter(revision) {
			return fmt.Errorf("revision can't contain control characters")
		}
		return nil
	}

	if bytes.HasPrefix(revision, []byte("-")) {
		return fmt.Errorf("revision can't start with '-'")
	}
	if bytes.Contains(revision, []byte("\x00")) {
		return fmt.Errorf("revision can't contain NUL")
	}

	if cfg.allowPathScopedRevision {
		// The path component, if any, may contain all bytes except for the NUL byte which we
		// already checked for above, and line breaks, which separate revisions on git's stdin.
		var path []byte
		revision, path, _ = bytes.Cut(revision, []byte(":"))
		if !cfg.allowLineBreaksInPath && bytes.ContainsAny(path, "\n\r") {
			return fmt.Errorf("path of a revision can't contain a line break")
		}
	}

	if !cfg.allowEmpty && len(revision) == 0 {
		return fmt.Errorf("empty revision")
	}
	if bytes.ContainsAny(revision, " \t\n\r") {
		return fmt.Errorf("revision can't contain whitespace")
	}
	if hasControlCharacter(revision) {
		return fmt.Errorf("revision can't contain control characters")
	}
	if bytes.Contains(revision, []byte(":")) {
		return fmt.Errorf("revision can't contain ':'")
	}
	if bytes.Contains(revision, []byte("\\")) {
		return fmt.Errorf("revision can't contain '\\'")
	}

	return nil
}

// isPseudoRevision reports whether revision is a pseudo-revision understood by git-rev-list(1) that
// AllowPseudoRevision allows.
func isPseudoRevision(revision []byte) bool {
	switch {
	case bytes.Equal(revision, []byte("--all")),
		bytes.Equal(revision, []byte("--not")),
		bytes.Equal(revision, []byte("--branches")),
		bytes.Equal(revision, []byte("--tags")),
		bytes.Equal(revision, []byte("--alternate-refs")),
		bytes.HasPrefix(revision, []byte("--branches=")),
		bytes.HasPrefix(revision, []byte("--tags=")),
		bytes.HasPrefix(revision, []byte("--glob=")):
		return true
	}
	return false
}

// hasControlCharacter reports whether b holds an ASCII control character: below space, or DEL.
// References cannot contain one (git check-ref-format), so no revision of a reference does.
func hasControlCharacter(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}
