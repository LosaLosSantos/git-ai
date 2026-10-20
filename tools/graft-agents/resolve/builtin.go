package resolve

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

// Built-in strategy types. They are registered in init so that any program
// importing this package can use them from configuration.
func init() {
	RegisterStrategy("trivial", newTrivial)
	RegisterStrategy("union", newUnion)
	RegisterStrategy("rerere", newRerere)
	RegisterStrategy("command", newCommand)
}

// DefaultCommandTimeout bounds one "command" strategy run when none is set.
const DefaultCommandTimeout = 2 * time.Minute

// trivial resolves only cases that need no content merge:
//   - both sides identical (including both deleted);
//   - one side unchanged against base (the other side wins, deletions too);
//   - both regular text files differ only in trailing whitespace or line
//     endings (the ours side is kept). Leading indentation is significant, so
//     an indentation change is never discarded, and binary files and symlinks
//     never take this rule.
type trivial struct{}

func newTrivial(spec StrategySpec) (Strategy, error) {
	if err := allowOnly(spec, "trivial"); err != nil {
		return nil, err
	}
	return trivial{}, nil
}

// IsRule: trivial is a rule (RuleStrategy).
func (trivial) IsRule() bool { return true }

func (trivial) Resolve(_ context.Context, f File, _ protocol.Request) (Resolution, bool, error) {
	switch {
	case sameSide(f.Ours, f.Theirs):
		return pick(f.Ours), true, nil
	case sameSide(f.Base, f.Theirs):
		return pick(f.Ours), true, nil
	case sameSide(f.Base, f.Ours):
		return pick(f.Theirs), true, nil
	case regularText(f.Ours) && regularText(f.Theirs) &&
		f.Ours.Mode == f.Theirs.Mode &&
		bytes.Equal(trimLineEnds(f.Ours.Content), trimLineEnds(f.Theirs.Content)):
		return pick(f.Ours), true, nil
	}
	return Resolution{}, false, nil
}

// sameSide reports whether two sides are equally present, with the same mode
// and content. Two absent sides are the same.
func sameSide(a, b *protocol.Side) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Mode == b.Mode && bytes.Equal(a.Content, b.Content)
}

// pick turns a side into an answer: an absent side means deletion.
func pick(s *protocol.Side) Resolution {
	if s == nil {
		return Resolution{Action: protocol.ActionDelete}
	}
	return Resolution{Action: protocol.ActionWrite, Mode: s.Mode, Content: s.Content}
}

// trimLineEnds removes trailing spaces, tabs and carriage returns from every
// line. Two texts that differ only in those bytes, or in line endings, have
// equal results. Leading whitespace is kept.
func trimLineEnds(b []byte) []byte {
	lines := bytes.Split(b, []byte("\n"))
	for i, line := range lines {
		lines[i] = bytes.TrimRight(line, " \t\r")
	}
	return bytes.Join(lines, []byte("\n"))
}

// regularFile reports whether a side is a regular (non-symlink) file.
func regularFile(s *protocol.Side) bool {
	return s != nil && (s.Mode == protocol.ModeFile || s.Mode == protocol.ModeExecutable)
}

// sideContent returns the content of a regular side, and nil for an absent side or one that is
// not a regular file (the syntax check skips those).
func sideContent(s *protocol.Side) []byte {
	if !regularFile(s) {
		return nil
	}
	if s.Content == nil {
		return []byte{}
	}
	return s.Content
}

// regularText reports whether a side is a regular file whose content is text
// (no NUL byte, the same heuristic as git).
func regularText(s *protocol.Side) bool {
	return regularFile(s) && !isBinary(s.Content)
}

// allowOnly rejects option keys the strategy does not know, so that typos in
// the configuration fail loudly.
func allowOnly(spec StrategySpec, allowed ...string) error {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var unknown []string
	for k := range spec.Options {
		if !ok[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("strategy %q (%s): unknown keys %v", spec.Name, spec.Type, unknown)
	}
	return nil
}

func optString(spec StrategySpec, key string) (string, error) {
	v, ok := spec.Options[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("strategy %q: %s must be a string", spec.Name, key)
	}
	return s, nil
}

func optStringList(spec StrategySpec, key string) ([]string, error) {
	v, ok := spec.Options[key]
	if !ok {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("strategy %q: %s must be a list of strings", spec.Name, key)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("strategy %q: %s must be a list of strings", spec.Name, key)
		}
		out = append(out, s)
	}
	return out, nil
}

func optDuration(spec StrategySpec, key string, def time.Duration) (time.Duration, error) {
	s, err := optString(spec, key)
	if err != nil {
		return 0, err
	}
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("strategy %q: invalid %s %q", spec.Name, key, s)
	}
	return d, nil
}
