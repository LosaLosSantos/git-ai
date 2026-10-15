// Package workspace implements a sparse, server-centric working copy on top of
// a Graft server. It materializes only the sparse part of a tree, fetches
// objects on demand through a local object cache and refuses to overwrite
// local changes when the tree is updated.
package workspace

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

var (
	// ErrNotFound is returned when a revision, path or branch does not exist.
	ErrNotFound = errors.New("not found")
	// ErrMergeConflict is returned by Backend.Land when the change cannot be
	// applied onto the current tip of the branch.
	ErrMergeConflict = errors.New("merge conflict")
	// ErrNotFastForward is returned by Backend.Land with the ff_only strategy
	// when the branch has diverged.
	ErrNotFastForward = errors.New("not a fast-forward")
	// ErrAlreadyExists is returned by Backend.CommitFiles when a file that is
	// not marked for overwrite already exists.
	ErrAlreadyExists = errors.New("already exists")
)

// Land strategies accepted by LandOptions.Strategy.
const (
	StrategyRebase = "rebase"
	StrategyMerge  = "merge"
	StrategyFFOnly = "ff_only"
)

// checkStrategy accepts the known land strategies. An empty name is accepted
// and means rebase.
func checkStrategy(strategy string) error {
	switch strategy {
	case "", StrategyRebase, StrategyMerge, StrategyFFOnly:
		return nil
	default:
		return fmt.Errorf("unknown land strategy %q (want rebase, merge or ff_only)", strategy)
	}
}

// ObjectType is the type of a Git object.
type ObjectType string

// Object types reported in tree entries.
const (
	ObjectBlob   ObjectType = "blob"
	ObjectTree   ObjectType = "tree"
	ObjectCommit ObjectType = "commit"
)

// Entry is one tree entry. Path is relative to the listed tree.
type Entry struct {
	Path string
	OID  string
	Type ObjectType
	Mode uint32
	Size int64
}

// Commit is the subset of commit metadata the CLI displays.
type Commit struct {
	ID         string
	AuthorName string
	Subject    string
}

// BlameLine attributes one line of a file to the commit that introduced it.
type BlameLine struct {
	LineNumber         int
	CommitID           string
	OriginalLineNumber int
	Content            []byte
}

// Changeset is one entry of the server's changeset log.
type Changeset struct {
	Sequence   uint64
	Reference  string
	OldOID     string
	NewOID     string
	Operation  string
	Actor      string
	RecordedAt time.Time
	Rewritten  bool
	// Members lists the changes that a batched land folded into this update, in the
	// order they were landed. It is empty for updates that are not batches.
	Members []ChangesetMember
}

// ChangesetMember is one change of a batched land, see Changeset.Members. Its
// resolved paths are kept byte for byte, so they may hold bytes that are not
// valid UTF-8.
type ChangesetMember struct {
	OriginalCommit string
	Commit         string
	Actor          string
	Rewritten      bool
	ResolvedPaths  []string
}

// ActionKind is the kind of a file action in a commit.
type ActionKind string

// File action kinds.
const (
	ActionPut    ActionKind = "put"
	ActionDelete ActionKind = "delete"
	ActionMove   ActionKind = "move"
	ActionChmod  ActionKind = "chmod"
)

// Action is one file change of a commit.
type Action struct {
	Kind       ActionKind
	Path       string
	FromPath   string // move only
	Content    []byte // put only
	Executable bool   // put and chmod
	Overwrite  bool   // put only: replace an existing file instead of creating one
}

// Signature is the author or committer of a commit.
type Signature struct {
	Name  string
	Email string
}

// CommitOptions describe a commit built on top of StartRev. The commit is
// written to Branch, which is created when it does not exist.
type CommitOptions struct {
	Branch   string
	StartRev string
	Message  string
	Author   Signature
	Actions  []Action
}

// LandOptions describe a request to land a commit onto a branch.
type LandOptions struct {
	Branch       string
	Commit       string
	Strategy     string
	CreateBranch bool
	// Committer is the identity recorded as committer of rewritten commits.
	Committer Signature
	// ResolveConflicts asks the server to resolve merge conflicts with its
	// conflict resolver.
	ResolveConflicts bool
}

// LandResult is the outcome of a land.
type LandResult struct {
	Commit        string
	AlreadyLanded bool
	Rewritten     bool
	// ResolvedPaths are the conflicting files resolved by the server's conflict
	// resolver, and Resolver its name. Both are empty without resolution.
	ResolvedPaths []string
	Resolver      string
	// BatchSize is the number of changes in the land batch the change was part
	// of, and BatchPosition its 1-based position in it. A change that landed
	// alone has 1 and 1; the zero values mean the batch is unknown.
	BatchSize     int32
	BatchPosition int32
	// Attempts is the number of compare-and-swap attempts the server used for the
	// land, batch rebuilds included. Zero means the server did not report it.
	Attempts int32
}

// Backend is the subset of the Graft SDK the workspace needs. It is bound to
// one repository. The sdk-backed implementation lives in cmd/graft; tests use
// MemoryBackend.
type Backend interface {
	// ResolveRevision resolves a branch, tag or commit-ish to a full commit ID.
	ResolveRevision(ctx context.Context, rev string) (string, error)
	// ListTree lists the entries under path at rev. Paths are relative to path.
	// It returns ErrNotFound if path does not exist.
	ListTree(ctx context.Context, rev, path string, recursive bool) ([]Entry, error)
	// GetObjects returns the contents of the requested blobs. Missing objects
	// are absent from the result.
	GetObjects(ctx context.Context, oids []string) (map[string][]byte, error)
	// ReadFile returns the content of a file at rev.
	ReadFile(ctx context.Context, rev, path string) ([]byte, error)
	// CommitFiles builds a commit from actions on top of StartRev, writes it to
	// opts.Branch and returns the new commit ID.
	CommitFiles(ctx context.Context, opts CommitOptions) (string, error)
	// Land lands a commit onto a branch, rebasing it when the branch diverged.
	Land(ctx context.Context, opts LandOptions) (LandResult, error)
	// DeleteBranch deletes a branch. Deleting a missing branch is not an error.
	DeleteBranch(ctx context.Context, branch string) error
	// Changesets returns up to limit changesets with sequence > after.
	Changesets(ctx context.Context, after uint64, limit int) ([]Changeset, error)
	// WatchChangesets streams the backlog after the cursor and then new
	// changesets until ctx is done. The error channel reports a failure: it is
	// sent before the changeset channel closes, and the error channel closes
	// when the watch ends.
	WatchChangesets(ctx context.Context, after uint64) (<-chan Changeset, <-chan error)
	// Blame attributes every line of a file at rev.
	Blame(ctx context.Context, rev, path string) ([]BlameLine, error)
	// PathHistory returns the commits that changed path, newest first.
	PathHistory(ctx context.Context, rev, path string, limit int) ([]Commit, error)
}

// Git file modes used in the workspace.
const (
	modeRegular uint32 = 0o100644
	modeExec    uint32 = 0o100755
	modeSymlink uint32 = 0o120000
	modeTree    uint32 = 0o040000
)

// BlobID returns the Git object ID of a blob with the given content, using
// SHA-1 or, when sha256Format is set, the SHA-256 object format.
func BlobID(content []byte, sha256Format bool) string {
	header := []byte("blob " + strconv.Itoa(len(content)) + "\x00")
	if sha256Format {
		h := sha256.New()
		_, _ = h.Write(header)
		_, _ = h.Write(content)
		return hex.EncodeToString(h.Sum(nil))
	}
	h := sha1.New()
	_, _ = h.Write(header)
	_, _ = h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// validOID reports whether oid is a full SHA-1 or SHA-256 object ID.
func validOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	_, err := hex.DecodeString(oid)
	return err == nil
}
