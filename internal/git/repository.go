package git

import "errors"

// DefaultBranch is the default reference written to HEAD when a repository is created
const DefaultBranch = "main"

// DefaultRef is the reference that GitLab will use if HEAD of the bare repository
// is not found, or other edge cases to detect the default branch.
const DefaultRef = ReferenceName("refs/heads/" + DefaultBranch)

// LegacyDefaultRef is the reference that used to be the default HEAD of the bare
// repository. If the default reference is not found, Gitaly will still test the
// legacy default.
const LegacyDefaultRef = ReferenceName("refs/heads/master")

// MirrorRefSpec is the refspec used when --mirror is specified on git clone.
const MirrorRefSpec = "+refs/*:refs/*"

var (
	// ErrReferenceNotFound represents an error when a reference was not
	// found.
	ErrReferenceNotFound = errors.New("reference not found")
	// ErrReferenceAmbiguous represents an error when a reference couldn't
	// unambiguously be resolved.
	ErrReferenceAmbiguous = errors.New("reference is ambiguous")

	// ErrAlreadyExists represents an error when the resource is already exists.
	ErrAlreadyExists = errors.New("already exists")
	// ErrNotFound represents an error when the resource can't be found.
	ErrNotFound = errors.New("not found")
)
