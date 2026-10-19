// Package resolver is the server side of the graft-resolve protocol v1 (see
// doc/graft/RESOLVE.md). It holds the request and response types, the
// acceptance rules the server applies to every answer (Validate), a Client
// that calls an operator-chosen resolver over a command or an HTTP endpoint,
// and the path and actor policy that decides which conflicts may be sent to
// it.
//
// The server never talks to an LLM or a vendor API: the resolver is any
// program or endpoint the operator configures.
package resolver

import (
	"encoding/base64"
	"encoding/json"
)

// ProtocolVersion is the only protocol version this package speaks.
const ProtocolVersion = 1

// MaxResolverNameBytes bounds Response.Resolver, the name of the resolver that
// the server writes into the Graft-Resolved-By commit trailer. A longer name is
// rejected. tools/graft-agents mirrors this value.
const MaxResolverNameBytes = 128

// File kinds of Request.Files.
const (
	KindContent      = "content"
	KindModifyDelete = "modify-delete"
	KindAddAdd       = "add-add"
	KindMode         = "mode"
)

// Actions of ResponseFile.
const (
	ActionWrite  = "write"
	ActionDelete = "delete"
)

// Git file modes accepted in responses.
const (
	ModeRegular    = "100644"
	ModeExecutable = "100755"
	ModeSymlink    = "120000"
)

// Request is the resolve request the server sends to the resolver.
// Ours is the current branch tip side, Theirs the change being landed. Any
// side may be nil (absent).
type Request struct {
	Version int `json:"version"`
	// Operation is OperationResolve (also when absent) or OperationCheck.
	Operation  string     `json:"operation,omitempty"`
	Repository Repository `json:"repository"`
	Branch     string     `json:"branch"`
	Tip        string     `json:"tip"`
	Commit     string     `json:"commit"`
	MergeBase  string     `json:"merge_base"`
	Messages   Messages   `json:"messages"`
	Actor      string     `json:"actor"`
	Files      []File     `json:"files"`
	// Resolution is, for OperationCheck only, the agent's resolution of Files to check.
	Resolution []ResponseFile `json:"resolution,omitempty"`
	Limits     Limits         `json:"limits"`
}

// Operations of a request.
const (
	// OperationResolve asks the resolver to resolve Files; an absent operation means it.
	OperationResolve = "resolve"
	// OperationCheck asks the resolver to check an agent's Resolution of Files with the checks of
	// its own answers, and to answer a CheckResponse.
	OperationCheck = "check"
)

// CheckResponse is the answer to an OperationCheck request: one verdict per file of the
// resolution, in its order.
type CheckResponse struct {
	Version   int    `json:"version"`
	Operation string `json:"operation"`
	// Resolver names the checker, for the Graft-Checked-By trailer.
	Resolver string      `json:"resolver"`
	Files    []CheckFile `json:"files"`
}

// Syntax verdicts of a CheckFile.
const (
	// SyntaxChecked: both sides parse, and so does the resolution.
	SyntaxChecked = "checked"
	// SyntaxUnchecked: a side does not parse with the grammar, so the resolution got no syntax
	// verdict.
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

// ErrorResponse is the body of a refusal: the HTTP 422 answer, or the stdout of
// a command that exits non-zero. Files is optional.
type ErrorResponse struct {
	Error string        `json:"error"`
	Files []FileOutcome `json:"files,omitempty"`
}

// Outcomes of a file in a refusal.
const (
	// OutcomeResolved: the resolver resolved the file, but not every file of
	// the request.
	OutcomeResolved = "resolved"
	// OutcomeRejected: the resolver's result for the file failed a check.
	OutcomeRejected = "rejected"
	// OutcomeFailed: the resolver failed on the file (a model error, a spent
	// budget, no strategy applying).
	OutcomeFailed = "failed"
)

// FileOutcome is what the resolver did with one file of a refused request.
type FileOutcome struct {
	Path    string `json:"path"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
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

// File is one conflicting path. Path must be valid UTF-8: JSON strings cannot
// carry arbitrary bytes, so a path that is not would reach the resolver under
// another name.
type File struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Base   *Side  `json:"base"`
	Ours   *Side  `json:"ours"`
	Theirs *Side  `json:"theirs"`
	// OursMessage is the commit message that explains OURS' change to this file: the newest commit of
	// the tip's side that touched it. The intent of OURS, as Messages.Commit is THEIRS'. Untrusted
	// data; empty when none was found.
	OursMessage string `json:"ours_message,omitempty"`
}

// Side is one version of a file. Content is base64 on the wire; a present
// side always carries a content string, possibly empty.
type Side struct {
	OID     string `json:"oid"`
	Mode    string `json:"mode"`
	Content []byte `json:"-"`
}

// sideJSON is the wire form of Side.
type sideJSON struct {
	OID     string `json:"oid"`
	Mode    string `json:"mode"`
	Content string `json:"content"`
}

// MarshalJSON encodes Content as base64, emitting "" rather than null for an
// empty present side.
func (s Side) MarshalJSON() ([]byte, error) {
	return json.Marshal(sideJSON{
		OID:     s.OID,
		Mode:    s.Mode,
		Content: base64.StdEncoding.EncodeToString(s.Content),
	})
}

// UnmarshalJSON decodes the base64 wire form of Side.
func (s *Side) UnmarshalJSON(data []byte) error {
	var raw sideJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	content, err := base64.StdEncoding.DecodeString(raw.Content)
	if err != nil {
		return err
	}

	*s = Side{OID: raw.OID, Mode: raw.Mode, Content: content}
	return nil
}

// Limits are the bounds the resolver must respect.
type Limits struct {
	MaxFileBytes int64 `json:"max_file_bytes"`
}

// Response is the resolver's answer.
type Response struct {
	Version     int            `json:"version"`
	Resolver    string         `json:"resolver"`
	Files       []ResponseFile `json:"files"`
	Explanation string         `json:"explanation"`
}

// The origins of a resolved file: what made the resolution.
const (
	// OriginRule: an operator's rule (trivial, union, rerere, command) made it. Only a file marked
	// as a rule lands without the operator's verification of the tree.
	OriginRule = "rule"
	// OriginModel: a model made it. It lands only if verify.command passed on the tree that lands
	// (PLAN10 14, 15.9), and is otherwise proposed to the agent.
	OriginModel = "model"
)

// ResponseFile is the resolution of one conflicting path. Content is base64
// on the wire and is only meaningful for ActionWrite.
//
// Origin says what made the resolution: OriginRule or OriginModel. It is fail-safe: a file is a
// rule's only when it says so, and a file without an origin, or with one that is none of the two,
// is a model's, which lands only through the operator's verification. Proposal is the older mark of
// a model's resolution, still read: it means OriginModel, whatever Origin says. The name of the
// resolver decides nothing.
type ResponseFile struct {
	Path     string `json:"path"`
	Action   string `json:"action"`
	Mode     string `json:"mode,omitempty"`
	Content  []byte `json:"content,omitempty"`
	Origin   string `json:"origin,omitempty"`
	Proposal bool   `json:"proposal,omitempty"`
	// Report is the composition report of a model's resolution (PLAN10 15.3): in exact words what it
	// keeps, drops and repeats of the two sides' changes. A report, never a gate: nothing in it
	// decides whether the resolution lands. It goes to the agent with the proposal.
	Report []string `json:"report,omitempty"`
}

// FromModel reports whether the file is a model's resolution: anything that is not marked as a
// rule's, and anything marked as a proposal.
func (f ResponseFile) FromModel() bool {
	return f.Proposal || f.Origin != OriginRule
}
