// Package protocol defines the graft-resolve wire protocol, version 1.
//
// The JSON shapes are the normative ones of doc/graft/PLAN2.md ("Resolve
// protocol v1"). The types are a deliberate copy of the server's: the
// tools/graft-agents module must never import the Gitaly module, so the
// JSON, not the Go type, is the contract.
//
// Byte contents are carried as base64 strings in JSON (Go's encoding of
// []byte).
package protocol

// Version is the only protocol version this package speaks.
const Version = 1

// Conflict kinds sent in FileConflict.Kind.
const (
	KindContent      = "content"
	KindModifyDelete = "modify-delete"
	KindAddAdd       = "add-add"
	KindMode         = "mode"
)

// Actions returned in ResolvedFile.Action.
const (
	ActionWrite  = "write"
	ActionDelete = "delete"
)

// File modes accepted by the server.
const (
	ModeFile       = "100644"
	ModeExecutable = "100755"
	ModeSymlink    = "120000"
)

// DefaultMaxFileBytes is used when a request carries no positive limit.
const DefaultMaxFileBytes int64 = 1 << 20

// DefaultMaxRequestBytes is the default bound on one request body in the
// command and HTTP modes. The server's default limits (20 files of 1 MiB, three
// sides each, base64 encoded) need about 80 MiB, so the default leaves room for
// them. Larger limits on the server need a larger bound, set by the operator.
const DefaultMaxRequestBytes int64 = 128 << 20

// MaxResolverNameBytes bounds Response.Resolver. The server writes the name
// into the Graft-Resolved-By commit trailer and rejects a longer one, which
// fails the whole resolution. It mirrors the server's limit.
const MaxResolverNameBytes = 128

// Request is the server-to-resolver message.
type Request struct {
	Version    int            `json:"version"`
	Repository Repository     `json:"repository"`
	Branch     string         `json:"branch"`
	Tip        string         `json:"tip"`
	Commit     string         `json:"commit"`
	MergeBase  string         `json:"merge_base"`
	Messages   Messages       `json:"messages"`
	Actor      string         `json:"actor"`
	Files      []FileConflict `json:"files"`
	Limits     Limits         `json:"limits"`
}

// Repository identifies the repository the conflict belongs to.
type Repository struct {
	Storage      string `json:"storage"`
	RelativePath string `json:"relative_path"`
}

// Messages carries the commit messages of both sides.
type Messages struct {
	Tip    string `json:"tip"`
	Commit string `json:"commit"`
}

// Limits carries the server-side bounds the answer must respect.
type Limits struct {
	MaxFileBytes int64 `json:"max_file_bytes"`
}

// FileConflict is one conflicted path. Base, Ours and Theirs may each be nil
// (absent on that side).
type FileConflict struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Base   *Side  `json:"base"`
	Ours   *Side  `json:"ours"`
	Theirs *Side  `json:"theirs"`
}

// Side is one version of a file.
type Side struct {
	OID     string `json:"oid"`
	Mode    string `json:"mode"`
	Content []byte `json:"content"`
}

// Response is the resolver-to-server message.
type Response struct {
	Version     int            `json:"version"`
	Resolver    string         `json:"resolver"`
	Files       []ResolvedFile `json:"files"`
	Explanation string         `json:"explanation,omitempty"`
}

// ResolvedFile is the answer for one conflicted path. Content is null for
// deletions; Mode is omitted for deletions.
type ResolvedFile struct {
	Path    string `json:"path"`
	Action  string `json:"action"`
	Mode    string `json:"mode,omitempty"`
	Content []byte `json:"content"`
}

// ErrorResponse is the body of an HTTP 422 (unresolvable) or other error.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ValidMode reports whether mode is one of the modes the server accepts.
func ValidMode(mode string) bool {
	switch mode {
	case ModeFile, ModeExecutable, ModeSymlink:
		return true
	}
	return false
}

// ValidKind reports whether kind is a known conflict kind.
func ValidKind(kind string) bool {
	switch kind {
	case KindContent, KindModifyDelete, KindAddAdd, KindMode:
		return true
	}
	return false
}
