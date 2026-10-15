package changelog

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// shortOIDLength is the number of characters of an object ID that FormatMembers
// shows.
const shortOIDLength = 8

// Member is one change that a batched land folded into a changeset. A batch
// lands several changes with a single reference update, so the changeset of the
// batch has one member per change. Members keep the per-change attribution that
// the single update would otherwise hide.
type Member struct {
	// OriginalCommit is the commit that the change submitted, before any rewrite.
	OriginalCommit string `json:"original_commit"`
	// Commit is the commit of the change inside the batch's chain of
	// commits, which is the rewritten commit when Rewritten is true.
	Commit string `json:"commit"`
	// Actor identifies who submitted the change. It is valid UTF-8.
	Actor string `json:"actor,omitempty"`
	// Rewritten is true when the change was rebased or merged onto the running tip
	// of the batch, so that Commit differs from OriginalCommit.
	Rewritten bool `json:"rewritten,omitempty"`
	// ResolvedPaths lists the paths that a resolver changed to land the change. It
	// is empty when the change landed without conflicts. Paths are kept byte for
	// byte, see MarshalJSON.
	ResolvedPaths []string `json:"resolved_paths,omitempty"`
}

// memberJSON is the JSON form of a Member.
type memberJSON struct {
	OriginalCommit string   `json:"original_commit"`
	Commit         string   `json:"commit"`
	Actor          string   `json:"actor,omitempty"`
	Rewritten      bool     `json:"rewritten,omitempty"`
	ResolvedPaths  []string `json:"resolved_paths,omitempty"`
	// ResolvedPathsBase64 holds every resolved path, base64 encoded and in the same
	// order as ResolvedPaths. It is only set when a path is not valid UTF-8, which
	// JSON strings cannot hold. ResolvedPaths then holds a readable but lossy form
	// of the paths, in which the invalid bytes are replaced with U+FFFD.
	ResolvedPathsBase64 []string `json:"resolved_paths_b64,omitempty"`
}

// MarshalJSON encodes the member with the keys original_commit, commit, actor,
// rewritten and resolved_paths. encoding/json would replace the invalid bytes of
// a resolved path with U+FFFD, so such paths are also encoded in
// resolved_paths_b64, which decodes them exactly.
func (m Member) MarshalJSON() ([]byte, error) {
	encoded := memberJSON{
		OriginalCommit: m.OriginalCommit,
		Commit:         m.Commit,
		Actor:          m.Actor,
		Rewritten:      m.Rewritten,
		ResolvedPaths:  m.ResolvedPaths,
	}

	if !allValidUTF8(m.ResolvedPaths) {
		encoded.ResolvedPaths = make([]string, len(m.ResolvedPaths))
		encoded.ResolvedPathsBase64 = make([]string, len(m.ResolvedPaths))
		for i, p := range m.ResolvedPaths {
			encoded.ResolvedPaths[i] = toValidUTF8(p)
			encoded.ResolvedPathsBase64[i] = base64.StdEncoding.EncodeToString([]byte(p))
		}
	}

	return json.Marshal(encoded)
}

// UnmarshalJSON decodes a member encoded by MarshalJSON.
func (m *Member) UnmarshalJSON(data []byte) error {
	var decoded memberJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	paths := decoded.ResolvedPaths
	if len(decoded.ResolvedPathsBase64) > 0 {
		if len(decoded.ResolvedPathsBase64) != len(decoded.ResolvedPaths) {
			return fmt.Errorf("resolved_paths_b64 has %d paths, resolved_paths has %d",
				len(decoded.ResolvedPathsBase64), len(decoded.ResolvedPaths))
		}

		paths = make([]string, len(decoded.ResolvedPathsBase64))
		for i, encoded := range decoded.ResolvedPathsBase64 {
			p, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return fmt.Errorf("decoding resolved_paths_b64: %w", err)
			}
			paths[i] = string(p)
		}
	}

	*m = Member{
		OriginalCommit: decoded.OriginalCommit,
		Commit:         decoded.Commit,
		Actor:          decoded.Actor,
		Rewritten:      decoded.Rewritten,
		ResolvedPaths:  paths,
	}

	return nil
}

// Resolved reports whether the member was landed with resolved conflicts.
func (m Member) Resolved() bool {
	return len(m.ResolvedPaths) > 0
}

// ValidateMembers checks that every member names both its original and its
// landed commit. It returns the first problem found, with the member's index.
func ValidateMembers(members []Member) error {
	for i, m := range members {
		if err := validateMember(m); err != nil {
			return fmt.Errorf("member %d: %w", i, err)
		}
	}

	return nil
}

// validateMember checks one member, see ValidateMembers.
func validateMember(m Member) error {
	switch {
	case m.OriginalCommit == "":
		return errors.New("original commit is empty")
	case m.Commit == "":
		return errors.New("commit is empty")
	case m.Rewritten && m.OriginalCommit == m.Commit:
		return errors.New("marked rewritten but commit equals original commit")
	}

	return nil
}

// validMembers returns the members that pass validateMember, in their order, and
// whether any member was dropped. Append uses it instead of failing: the
// reference update it records has already happened, so its entry must still be
// recorded, and the dropped members are flagged with MembersTruncated.
func validMembers(members []Member) ([]Member, bool) {
	if len(members) == 0 {
		return nil, false
	}

	valid := make([]Member, 0, len(members))
	for _, m := range members {
		if validateMember(m) == nil {
			valid = append(valid, m)
		}
	}

	return valid, len(valid) != len(members)
}

// normalizeMembers returns a copy of members in which the actors are valid UTF-8,
// as Entry actors must be. Resolved paths are copied as they are, because they
// are encoded exactly by MarshalJSON. It returns nil for an empty input so that
// an entry without members encodes without the field.
func normalizeMembers(members []Member) []Member {
	if len(members) == 0 {
		return nil
	}

	out := make([]Member, len(members))
	for i, m := range members {
		out[i] = Member{
			OriginalCommit: m.OriginalCommit,
			Commit:         m.Commit,
			Actor:          toValidUTF8(m.Actor),
			Rewritten:      m.Rewritten,
			ResolvedPaths:  append([]string(nil), m.ResolvedPaths...),
		}
	}

	return out
}

// allValidUTF8 reports whether every string of ss is valid UTF-8.
func allValidUTF8(ss []string) bool {
	for _, s := range ss {
		if !utf8.ValidString(s) {
			return false
		}
	}

	return true
}

// toValidUTF8 replaces invalid bytes with U+FFFD, like Append does for actors.
func toValidUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, string(utf8.RuneError))
}

// FormatMembers renders members as one line each, for the SDK and the CLI. A
// line reads "<original> -> <landed> by <actor>", followed by " rewritten" and
// " resolved: <paths>" when those apply. Object IDs are shortened.
func FormatMembers(members []Member) []string {
	lines := make([]string, 0, len(members))
	for _, m := range members {
		var b strings.Builder
		fmt.Fprintf(&b, "%s -> %s", shortOID(m.OriginalCommit), shortOID(m.Commit))
		if m.Actor != "" {
			fmt.Fprintf(&b, " by %s", m.Actor)
		}
		if m.Rewritten {
			b.WriteString(" rewritten")
		}
		if m.Resolved() {
			fmt.Fprintf(&b, " resolved: %s", strings.Join(m.ResolvedPaths, ", "))
		}
		lines = append(lines, b.String())
	}

	return lines
}

// shortOID returns the first shortOIDLength characters of oid, or oid itself
// when it is shorter.
func shortOID(oid string) string {
	if len(oid) <= shortOIDLength {
		return oid
	}
	return oid[:shortOIDLength]
}
