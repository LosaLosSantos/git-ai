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
	Version    int        `json:"version"`
	Repository Repository `json:"repository"`
	Branch     string     `json:"branch"`
	Tip        string     `json:"tip"`
	Commit     string     `json:"commit"`
	MergeBase  string     `json:"merge_base"`
	Messages   Messages   `json:"messages"`
	Actor      string     `json:"actor"`
	Files      []File     `json:"files"`
	Limits     Limits     `json:"limits"`
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

// ResponseFile is the resolution of one conflicting path. Content is base64
// on the wire and is only meaningful for ActionWrite.
type ResponseFile struct {
	Path    string `json:"path"`
	Action  string `json:"action"`
	Mode    string `json:"mode,omitempty"`
	Content []byte `json:"content,omitempty"`
}
