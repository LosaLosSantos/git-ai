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
	Version int `json:"version"`
	// Operation is OperationResolve (also when absent) or OperationCheck.
	Operation  string         `json:"operation,omitempty"`
	Repository Repository     `json:"repository"`
	Branch     string         `json:"branch"`
	Tip        string         `json:"tip"`
	Commit     string         `json:"commit"`
	MergeBase  string         `json:"merge_base"`
	Messages   Messages       `json:"messages"`
	Actor      string         `json:"actor"`
	Files      []FileConflict `json:"files"`
	// Resolution is, for OperationCheck only, the agent's resolution of Files to check.
	Resolution []ResolvedFile `json:"resolution,omitempty"`
	Limits     Limits         `json:"limits"`
}

// Operations of a request.
const (
	// OperationResolve asks for a resolution of Files; an absent operation means it.
	OperationResolve = "resolve"
	// OperationCheck asks to check an agent's Resolution of Files with the checks of the resolver's
	// own answers, answered with a CheckResponse.
	OperationCheck = "check"
)

// CheckResponse is the answer to an OperationCheck request: one verdict per file of the
// resolution, in its order.
type CheckResponse struct {
	Version   int    `json:"version"`
	Operation string `json:"operation"`
	// Resolver names the checker, for the Graft-Checked-By trailer of the server.
	Resolver string      `json:"resolver"`
	Files    []CheckFile `json:"files"`
}

// Syntax verdicts of a CheckFile.
const (
	// SyntaxChecked: both sides parse, and so does the resolution.
	SyntaxChecked = "checked"
	// SyntaxUnchecked: a side does not parse with the grammar, so there is no syntax verdict.
	SyntaxUnchecked = "unchecked"
	// SyntaxNone: the path has no grammar, or the file is deleted.
	SyntaxNone = "none"
)

// CheckFile is the verdict on one file of a resolution.
type CheckFile struct {
	Path   string `json:"path"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	Syntax string `json:"syntax"`
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
	// Files is the outcome of every file of a refused request (optional). The server uses it only
	// when it has exactly one entry per requested path, each with a known outcome.
	Files []FileOutcome `json:"files,omitempty"`
}

// Outcomes of a file in a refusal.
const (
	// OutcomeResolved: the file was resolved, but not every file of the request.
	OutcomeResolved = "resolved"
	// OutcomeRejected: a strategy's result for the file failed a rule or a check.
	OutcomeRejected = "rejected"
	// OutcomeFailed: the file could not be resolved (no strategy applied, a model error, a spent
	// AI budget).
	OutcomeFailed = "failed"
)

// FileOutcome is what the resolver did with one file of a refused request.
type FileOutcome struct {
	Path    string `json:"path"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
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
