package superfan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultMaxDetailsBytes bounds the answer of a details command.
const DefaultMaxDetailsBytes = 4 << 20

// ErrDetailsTooLarge is returned, wrapped, when the answer of a details command
// exceeds its byte limit. The commit is then too large to inspect, which is a
// finding in itself (see superfan.handle).
var ErrDetailsTooLarge = errors.New("details output too large")

// MaxDetailFiles bounds the number of files kept from one commit.
const MaxDetailFiles = 100000

// FileChange is one file changed by a commit.
type FileChange struct {
	Path      string `json:"path"`
	Status    string `json:"status,omitempty"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// Details describe the commit of an event. The JSON form is the output of
// `graft show --json REV` (see doc.go).
type Details struct {
	OID string `json:"oid"`
	// PeeledFrom is the object that `graft show` peeled to the commit OID, when
	// it was asked for another object: an annotated tag, whose ref update has
	// the tag's ID as new_oid. It is empty otherwise.
	PeeledFrom string              `json:"peeled_from,omitempty"`
	Parents    []string            `json:"parents"`
	Author     string              `json:"author"`
	Subject    string              `json:"subject"`
	Message    string              `json:"message"`
	Trailers   map[string][]string `json:"trailers"`
	Files      []FileChange        `json:"files"`
}

// StatusRenamedFrom is the status of the old path of a rename. `graft show`
// lists a rename as two files, the new path with status "renamed" and the old
// path with this status, so that path rules see both sides.
const StatusRenamedFrom = "renamed-from"

// ChangedFiles returns the number of files the commit changes. The old path of
// a rename (StatusRenamedFrom) is not counted: a rename is one changed file.
func (d Details) ChangedFiles() int {
	n := 0
	for _, f := range d.Files {
		if f.Status != StatusRenamedFrom {
			n++
		}
	}
	return n
}

// Lines returns the number of added and deleted lines.
func (d Details) Lines() int {
	n := 0
	for _, f := range d.Files {
		n += f.Additions + f.Deletions
	}
	return n
}

// Trailer returns the first value of a trailer and whether it is present.
func (d Details) Trailer(key string) (string, bool) {
	values := d.Trailers[key]
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// ParseDetails decodes and validates a Details object for the event's commit.
//
// The object must describe oid: either its "oid" is oid, or oid is the object
// that was peeled to that commit ("peeled_from", an annotated tag).
//
// The paths of the files come from the commit, so they are whatever its author
// chose, and git accepts nearly anything in a path (backslashes, tabs,
// newlines). They are kept as they are: one odd path must not make the whole
// commit unreadable, or it would hide the commit's other files from the
// protected-path rule and from the components. The rules match them with
// MatchFile, and everything that shows them sanitizes them (oneLine).
//
// A list of more than MaxDetailFiles files is ErrDetailsTooLarge: the commit is
// too large to inspect, like an answer over the byte limit.
func ParseDetails(data []byte, oid string) (Details, error) {
	var d Details
	if err := json.Unmarshal(data, &d); err != nil {
		return Details{}, fmt.Errorf("decode details: %w", err)
	}
	if d.OID != oid && (d.PeeledFrom == "" || d.PeeledFrom != oid) {
		return Details{}, fmt.Errorf("details are for %q, want %q", oneLine(d.OID, maxLabelText), oid)
	}
	if len(d.Files) > MaxDetailFiles {
		return Details{}, fmt.Errorf("%w: details list %d files, more than %d", ErrDetailsTooLarge, len(d.Files), MaxDetailFiles)
	}
	for _, f := range d.Files {
		if f.Additions < 0 || f.Deletions < 0 {
			return Details{}, fmt.Errorf("file %q: negative line counts", oneLine(f.Path, maxPathText))
		}
	}
	return d, nil
}

// DetailSource fetches the details of an event's commit.
type DetailSource interface {
	Details(ctx context.Context, ev Event) (Details, error)
}

// CommandDetails fetches details by running a command such as
// `graft show --json {oid}`. Every "{oid}" in an argument is replaced by the
// commit ID. There is no shell. The command must print one Details object.
type CommandDetails struct {
	Argv     []string
	Timeout  time.Duration
	MaxBytes int64
}

// Details runs the command for ev and parses its output.
func (c CommandDetails) Details(ctx context.Context, ev Event) (Details, error) {
	if len(c.Argv) == 0 {
		return Details{}, errors.New("superfan: empty details command")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxBytes := c.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxDetailsBytes
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := make([]string, len(c.Argv))
	for i, a := range c.Argv {
		argv[i] = strings.ReplaceAll(a, "{oid}", ev.NewOID)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout bytes.Buffer
	stderr := &cappedBuffer{max: maxCommandStderr}
	out := &limitWriter{buf: &stdout, max: maxBytes}
	cmd.Stdout = out
	cmd.Stderr = stderr
	// The limit stops the command's pipe, so the command usually fails too. The
	// overflow is checked first: it is the reason, whatever the exit status says.
	runErr := cmd.Run()
	if out.exceeded {
		return Details{}, fmt.Errorf("%w: superfan: details output exceeds %d bytes", ErrDetailsTooLarge, maxBytes)
	}
	if runErr != nil {
		if msg := bytes.TrimSpace(stderr.buf.Bytes()); len(msg) > 0 {
			return Details{}, fmt.Errorf("superfan: details command failed: %w: %s", runErr, msg)
		}
		return Details{}, fmt.Errorf("superfan: details command failed: %w", runErr)
	}
	return ParseDetails(stdout.Bytes(), ev.NewOID)
}

// limitWriter keeps at most max bytes and reports an error beyond that, which
// stops the command's pipe. exceeded records the overflow.
type limitWriter struct {
	buf      *bytes.Buffer
	max      int64
	exceeded bool
}

var errOutputTooLarge = errors.New("output too large")

func (w *limitWriter) Write(p []byte) (int, error) {
	if int64(w.buf.Len())+int64(len(p)) > w.max {
		w.exceeded = true
		return 0, errOutputTooLarge
	}
	return w.buf.Write(p)
}
