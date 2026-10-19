package resolver

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"strings"
)

// Validate applies the server's acceptance rules to a resolver answer. An
// answer is accepted only if:
//   - its version is ProtocolVersion;
//   - every conflicted path of the request is answered exactly once and no
//     other path appears;
//   - every action is "write" or "delete";
//   - every written mode is one of 100644, 100755, 120000;
//   - no written content exceeds req.Limits.MaxFileBytes;
//   - no written text file (content without NUL bytes) contains a conflict
//     marker line ("<<<<<<< ", "=======", ">>>>>>> " at line start);
//   - no written file contains a line starting with ConflictPointerPrefix,
//     unless a side of its conflict has such a line of its own.
//
// Violations are reported wrapping ErrInvalidResponse. A request without a
// positive req.Limits.MaxFileBytes is a caller error and is reported without
// that wrapping.
func Validate(req *Request, resp *Response) error {
	if req == nil || resp == nil {
		return fmt.Errorf("%w: nil request or response", ErrInvalidResponse)
	}

	if req.Limits.MaxFileBytes <= 0 {
		return errors.New("validate resolver response: request has no max_file_bytes limit")
	}

	if resp.Version != ProtocolVersion {
		return fmt.Errorf("%w: version %d, want %d", ErrInvalidResponse, resp.Version, ProtocolVersion)
	}

	conflicted := make(map[string]struct{}, len(req.Files))
	for _, f := range req.Files {
		conflicted[f.Path] = struct{}{}
	}

	answered := make(map[string]struct{}, len(resp.Files))
	for _, f := range resp.Files {
		if _, ok := conflicted[f.Path]; !ok {
			return fmt.Errorf("%w: path %q is not a conflicted path of the request", ErrInvalidResponse, f.Path)
		}

		if _, dup := answered[f.Path]; dup {
			return fmt.Errorf("%w: path %q is answered more than once", ErrInvalidResponse, f.Path)
		}
		answered[f.Path] = struct{}{}

		if err := validateFile(f, req.Limits.MaxFileBytes, sidesHavePointer(req.Files, f.Path)); err != nil {
			return fmt.Errorf("%w: path %q: %w", ErrInvalidResponse, f.Path, err)
		}
	}

	// Iterate the request's slice, not the map, so the error is deterministic.
	for _, f := range req.Files {
		if _, ok := answered[f.Path]; !ok {
			return fmt.Errorf("%w: conflicted path %q is not answered", ErrInvalidResponse, f.Path)
		}
	}

	return nil
}

// ConflictPointerPrefix starts the line that graft land --conflicts-dir writes in the file to
// edit in place of a binary side, which that file cannot embed. A resolution that still holds such
// a line would land the pointer instead of the side, so no written file may hold one, unless a
// side of its conflict has such a line of its own (as the patch-lines rule of graft-resolve does).
const ConflictPointerPrefix = "graft-conflict-pointer: "

// ErrConflictPointer is the rejection of a written file that holds a pointer line. A caller that
// did not give the sides (ValidateFile) may read them and validate again with
// ValidateFileWithSides, which exempts a file whose sides have such a line.
var ErrConflictPointer = errors.New("the pointer to a binary side that graft land --conflicts-dir writes was not replaced by the side")

// ValidateFile applies the acceptance rules of Validate to one resolved file: the action, the
// mode, the size, the conflict markers and the pointer lines. The server applies it to an agent's
// resolution too.
func ValidateFile(f ResponseFile, maxBytes int64) error {
	return validateFile(f, maxBytes, false)
}

// ValidateFileWithSides is ValidateFile for a file whose conflict has the given sides: a pointer
// line is accepted when one of them has one too.
func ValidateFileWithSides(f ResponseFile, maxBytes int64, sides ...[]byte) error {
	pointerAllowed := false
	for _, s := range sides {
		pointerAllowed = pointerAllowed || HasConflictPointer(s)
	}
	return validateFile(f, maxBytes, pointerAllowed)
}

// sidesHavePointer reports whether a side of the conflict of path in files has a pointer line.
func sidesHavePointer(files []File, path string) bool {
	for _, f := range files {
		if f.Path != path {
			continue
		}
		for _, s := range []*Side{f.Base, f.Ours, f.Theirs} {
			if s != nil && HasConflictPointer(s.Content) {
				return true
			}
		}
	}
	return false
}

// validateFile checks one answered file. pointerAllowed exempts it from the pointer-line rule.
func validateFile(f ResponseFile, maxBytes int64, pointerAllowed bool) error {
	switch f.Action {
	case ActionWrite:
		if !validMode(f.Mode) {
			return fmt.Errorf("invalid mode %q for write", f.Mode)
		}

		if f.Mode == ModeSymlink {
			if special := gitSpecialFile(path.Base(f.Path)); special != "" {
				return fmt.Errorf("a symbolic link named %s is refused: it stands for %s, and git fsck and checkout do not allow .gitmodules, .gitattributes, .gitignore or .mailmap to be a symbolic link", path.Base(f.Path), special)
			}
		}

		if int64(len(f.Content)) > maxBytes {
			return fmt.Errorf("content is %d bytes, limit is %d", len(f.Content), maxBytes)
		}

		if !isBinary(f.Content) && hasConflictMarker(f.Content) {
			return errors.New("content contains a conflict marker line")
		}

		if !pointerAllowed && HasConflictPointer(f.Content) {
			return fmt.Errorf("content contains a line starting with %q: %w", ConflictPointerPrefix, ErrConflictPointer)
		}

		return nil
	case ActionDelete:
		if len(f.Content) > 0 {
			return errors.New("delete must not carry content")
		}

		if f.Mode != "" && !validMode(f.Mode) {
			return fmt.Errorf("invalid mode %q for delete", f.Mode)
		}

		return nil
	default:
		return fmt.Errorf("invalid action %q", f.Action)
	}
}

// gitSpecialFiles are the files git reads from the tree itself, which must not be symbolic links
// (git fsck: gitmodulesSymlink, gitattributesSymlink, gitignoreSymlink, mailmapSymlink), each with
// the stem of its NTFS short name when it is not the first six letters of its name.
var gitSpecialFiles = []struct {
	name, shortHash string
}{
	{".gitmodules", "gi7eba"},
	{".gitattributes", "gi7d29"},
	{".gitignore", "gi250a"},
	{".mailmap", "maba30"},
}

// gitSpecialFile returns the special file that the tree entry name stands for on some file
// system, or "". It follows the rules git uses for these names:
//   - HFS+: the name, ignoring the code points HFS+ ignores (zero-width and directional marks,
//     U+FEFF), case-insensitively;
//   - NTFS (ntfsSpecialName): the name followed by spaces and periods only, the 8.3 short name
//     (the first six letters, "~" and a digit 1 to 4), and the hashed short name (up to six
//     letters of the stem above, "~", a digit 1 to 9 and more digits, eight characters in all),
//     each followed by spaces and periods only. An alternate data stream (":stream") is cut
//     first, which refuses more than git does.
func gitSpecialFile(name string) string {
	folded := FoldGitName(name)
	ntfs := ntfsName(name)
	for _, s := range gitSpecialFiles {
		if folded == s.name || ntfsSpecialName(ntfs, s.name[1:], s.shortHash) {
			return s.name
		}
	}
	return ""
}

// FoldGitName is the name as a case-insensitive file system that ignores the HFS+ ignorable code
// points sees it: lower case, without those code points. Two names that fold to the same string
// may be one name on such a file system.
func FoldGitName(name string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		if hfsIgnorable(r) {
			return -1
		}
		return r
	}, name))
}

// ntfsName is the lower-case name without an alternate data stream (":stream"), as the NTFS
// rules read it.
func ntfsName(name string) string {
	ntfs := strings.ToLower(name)
	if i := strings.IndexByte(ntfs, ':'); i >= 0 {
		ntfs = ntfs[:i]
	}
	return ntfs
}

// IsGitDirName reports whether the path component name stands for ".git" on some file system, by
// the rules of git's verify_path: ".git" case-insensitively, ignoring the code points HFS+
// ignores, and on NTFS ".git" or its 8.3 short name "git~1" followed by spaces and periods only.
// A tree is never to hold such a component, as git would treat the directory as the repository's
// own: its config and hooks would run.
func IsGitDirName(name string) bool {
	if FoldGitName(name) == ".git" {
		return true
	}
	ntfs := ntfsName(name)
	for _, prefix := range []string{".git", "git~1"} {
		if strings.HasPrefix(ntfs, prefix) && onlySpacesAndPeriods(ntfs[len(prefix):]) {
			return true
		}
	}
	return false
}

// ntfsSpecialName reports whether the lower-case name stands on NTFS for "." + dotName, whose
// hashed 8.3 short name starts with shortHash (see gitSpecialFile).
func ntfsSpecialName(name, dotName, shortHash string) bool {
	if strings.HasPrefix(name, "."+dotName) {
		return onlySpacesAndPeriods(name[1+len(dotName):])
	}
	if len(name) >= 8 && name[:6] == dotName[:6] && name[6] == '~' && name[7] >= '1' && name[7] <= '4' {
		return onlySpacesAndPeriods(name[8:])
	}
	sawTilde := false
	i := 0
	for ; i < 8; i++ {
		if i >= len(name) {
			return false
		}
		c := name[i]
		switch {
		case sawTilde:
			if c < '0' || c > '9' {
				return false
			}
		case c == '~':
			i++
			if i >= len(name) || name[i] < '1' || name[i] > '9' {
				return false
			}
			sawTilde = true
		case i >= 6 || c != shortHash[i]:
			return false
		}
	}
	return onlySpacesAndPeriods(name[i:])
}

// onlySpacesAndPeriods reports whether s holds nothing but spaces and periods.
func onlySpacesAndPeriods(s string) bool {
	return strings.Trim(s, " .") == ""
}

// hfsIgnorable reports whether HFS+ ignores r when it compares names.
func hfsIgnorable(r rune) bool {
	switch {
	case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
		return true
	}
	return false
}

// validMode reports whether mode is a git file mode the server accepts.
func validMode(mode string) bool {
	switch mode {
	case ModeRegular, ModeExecutable, ModeSymlink:
		return true
	default:
		return false
	}
}

// isBinary reports whether content is treated as binary: it holds a NUL byte.
func isBinary(content []byte) bool {
	return bytes.IndexByte(content, 0) >= 0
}

// HasConflictPointer reports whether a line of content starts with ConflictPointerPrefix.
func HasConflictPointer(content []byte) bool {
	prefix := []byte(ConflictPointerPrefix)
	for len(content) > 0 {
		line := content
		if i := bytes.IndexByte(content, '\n'); i >= 0 {
			line, content = content[:i], content[i+1:]
		} else {
			content = nil
		}
		if bytes.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// hasConflictMarker reports whether a line of content starts with a conflict
// marker: "<<<<<<< " or ">>>>>>> " followed by a label, or "=======" optionally
// followed by whitespace only. A trailing carriage return is ignored.
func hasConflictMarker(content []byte) bool {
	for len(content) > 0 {
		line := content
		if i := bytes.IndexByte(content, '\n'); i >= 0 {
			line, content = content[:i], content[i+1:]
		} else {
			content = nil
		}

		line = bytes.TrimSuffix(line, []byte{'\r'})

		switch {
		case bytes.HasPrefix(line, []byte("<<<<<<< ")):
			return true
		case bytes.HasPrefix(line, []byte(">>>>>>> ")):
			return true
		case bytes.HasPrefix(line, []byte("=======")) && len(bytes.TrimSpace(line[len("======="):])) == 0:
			return true
		}
	}

	return false
}
