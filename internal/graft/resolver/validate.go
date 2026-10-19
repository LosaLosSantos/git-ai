package resolver

import (
	"bytes"
	"errors"
	"fmt"
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
//     marker line ("<<<<<<< ", "=======", ">>>>>>> " at line start).
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

		if err := validateFile(f, req.Limits.MaxFileBytes); err != nil {
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

// ValidateFile applies the acceptance rules of Validate to one resolved file: the action, the
// mode, the size and the conflict markers. The server applies it to an agent's resolution too.
func ValidateFile(f ResponseFile, maxBytes int64) error {
	return validateFile(f, maxBytes)
}

// validateFile checks one answered file.
func validateFile(f ResponseFile, maxBytes int64) error {
	switch f.Action {
	case ActionWrite:
		if !validMode(f.Mode) {
			return fmt.Errorf("invalid mode %q for write", f.Mode)
		}

		if int64(len(f.Content)) > maxBytes {
			return fmt.Errorf("content is %d bytes, limit is %d", len(f.Content), maxBytes)
		}

		if !isBinary(f.Content) && hasConflictMarker(f.Content) {
			return errors.New("content contains a conflict marker line")
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
