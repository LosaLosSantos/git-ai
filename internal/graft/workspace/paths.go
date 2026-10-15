package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// metaDirName is the directory inside a workspace that holds its state and
// object cache. It is never materialized from the repository.
const metaDirName = ".graft"

// reservedComponent reports whether a path component is never read from or
// written to the workspace. The Git directory and the graft metadata directory
// are reserved at any depth, as Git reserves ".git". A nested ".graft" directory
// of the repository is not materialized, because Find would take a
// ".graft/state.json" in it for the state of a workspace rooted there, with a
// server address and repository chosen by whoever wrote the file. The
// comparison ignores case, because the workspace may live on a case-insensitive
// file system where ".GRAFT" and ".graft" are the same name.
func reservedComponent(component string) bool {
	return strings.EqualFold(component, ".git") || strings.EqualFold(component, metaDirName)
}

// isReservedPath reports whether a slash-separated, workspace-relative path has a
// reserved component.
func isReservedPath(rel string) bool {
	for _, component := range strings.Split(rel, "/") {
		if reservedComponent(component) {
			return true
		}
	}
	return false
}

// ValidatePath checks a slash-separated, repository-relative path. It rejects
// absolute paths, empty components, "." and "..", NUL bytes, reserved names and,
// on Windows, names that are not safe Windows file names. Backslashes and colons
// are ordinary characters of Git paths and of file names elsewhere, so they are
// rejected only on Windows, where they separate paths and name drives or
// alternate data streams. The returned path has no trailing slash.
func ValidatePath(p string) (string, error) {
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", errors.New("empty path")
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("invalid path %q", p)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("absolute path %q is not allowed", p)
	}
	for _, component := range strings.Split(p, "/") {
		switch {
		case component == "", component == ".", component == "..":
			return "", fmt.Errorf("invalid path %q", p)
		case reservedComponent(component):
			return "", fmt.Errorf("reserved path %q", p)
		case runtime.GOOS == "windows" && windowsNameProblem(component):
			return "", fmt.Errorf("path %q is not a safe Windows file name", p)
		}
	}
	return p, nil
}

// windowsDeviceNames are the device names that Windows resolves in every
// directory, with or without an extension.
var windowsDeviceNames = func() map[string]bool {
	names := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}
	for i := 1; i <= 9; i++ {
		names[fmt.Sprintf("COM%d", i)] = true
		names[fmt.Sprintf("LPT%d", i)] = true
	}
	return names
}()

// windowsNameProblem reports whether a path component would not name the file it
// spells on Windows. A backslash separates paths and a colon names a drive or an
// alternate data stream; the other reserved characters and control characters
// cannot be written at all. Windows strips trailing dots and spaces, so
// ".graft." and ".graft " would write into the metadata directory. It resolves
// device names such as NUL or COM1, and 8.3 short names such as GRAFT~1 alias
// other names.
func windowsNameProblem(component string) bool {
	if strings.ContainsAny(component, `\:<>"|?*`) {
		return true
	}
	for _, r := range component {
		if r < 0x20 {
			return true
		}
	}
	if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
		return true
	}
	for i := 0; i+1 < len(component); i++ {
		if component[i] == '~' && component[i+1] >= '0' && component[i+1] <= '9' {
			return true
		}
	}
	stem, _, _ := strings.Cut(component, ".")
	return windowsDeviceNames[strings.ToUpper(stem)]
}

// localPath maps a validated repository path to a location inside root. It
// refuses paths whose existing components are symbolic links or non-directories,
// so a write can never escape the workspace root.
func localPath(root, rel string) (string, error) {
	clean, err := ValidatePath(rel)
	if err != nil {
		return "", err
	}
	cur := root
	for _, part := range strings.Split(clean, "/") {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("path %q is, or goes through, a symbolic link (symbolic links are not supported yet)", clean)
		}
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	inside, err := filepath.Rel(root, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", clean)
	}
	return full, nil
}

// normalizeSparse validates sparse prefixes, removes duplicates and sorts them.
// A prefix that selects the whole repository makes the result nil (full checkout).
func normalizeSparse(prefixes []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, p := range prefixes {
		trimmed := strings.Trim(strings.TrimSpace(p), "/")
		if trimmed == "" || trimmed == "." {
			return nil, nil
		}
		clean, err := ValidatePath(trimmed)
		if err != nil {
			return nil, fmt.Errorf("sparse prefix: %w", err)
		}
		if !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	sort.Strings(out)
	return out, nil
}

// joinRel joins a sparse prefix and a path relative to it.
func joinRel(prefix, rel string) string {
	if prefix == "" {
		return rel
	}
	return prefix + "/" + rel
}
