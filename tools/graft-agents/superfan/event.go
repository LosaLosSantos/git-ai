package superfan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// MaxEventLine bounds one JSON line of the event stream.
const MaxEventLine = 64 << 10

// Operations and actors of the changeset log that the rules know about.
const (
	// OperationLand is the operation of a change landed on its own.
	OperationLand = "land"
	// OperationLandBatch is the operation of a batched land: several changes landed
	// with one reference update. Its changeset lists them in Members.
	OperationLandBatch = "land_batch"
	// BatchActor is the actor of the changeset of a batched land. It is the
	// identity of the combined update, not a requester: the requesters are the
	// actors of the members.
	BatchActor = "graft-batch"
)

// Event is one line of `graft watch --json`: one entry of the server's
// changeset log. The field names are the wire format (see doc.go).
type Event struct {
	Sequence   uint64    `json:"sequence"`
	Reference  string    `json:"reference"`
	OldOID     string    `json:"old_oid"`
	NewOID     string    `json:"new_oid"`
	Operation  string    `json:"operation"`
	Actor      string    `json:"actor"`
	RecordedAt time.Time `json:"recorded_at"`
	Rewritten  bool      `json:"rewritten"`
	// Members are the changes that a batched land (OperationLandBatch) folded
	// into the changeset, in landing order. Actor is then BatchActor, and the
	// members carry the actors of the requests. The rules judge every member as
	// a change of its own (see batchUnits). Empty for other operations.
	Members []Member `json:"members,omitempty"`

	// part is set on the events that batchUnits derives from a batched land.
	part batchPart
}

// Member is one change of a batched land, as the changeset log records it.
type Member struct {
	// OriginalCommit is the commit that the request submitted.
	OriginalCommit string `json:"original_commit"`
	// Commit is the commit the change landed as, in the batch's chain of
	// commits. It differs from OriginalCommit when Rewritten is true.
	Commit string `json:"commit"`
	// Actor identifies who submitted the change.
	Actor string `json:"actor,omitempty"`
	// Rewritten is true when the change was rebased or merged onto the commit
	// of the member before it.
	Rewritten bool `json:"rewritten,omitempty"`
	// ResolvedPaths are the paths that a resolver changed to land the change.
	ResolvedPaths []string `json:"resolved_paths,omitempty"`
}

// batchPart tells which part of a batched land an event derived by batchUnits
// stands for.
type batchPart uint8

const (
	// wholeEvent is an event as read from the stream.
	wholeEvent batchPart = iota
	// memberPart is one member of a batched land.
	memberPart
	// unlistedPart is the tip of a batched land whose member list is
	// incomplete. Its actor is BatchActor: the actors of the unlisted changes
	// are unknown.
	unlistedPart
)

// batchUnits returns the changes of ev that the rules judge one by one.
//
// An event other than a batched land is one change. Before batching, every
// request landed with a changeset of its own, which the rules judged with its
// actor and commit. A batched land keeps that: it is one change per member,
// with the member's actor, its commit as NewOID, the commit of the member
// before it (or the batch's OldOID) as OldOID, and its rewrite flag. Its own
// actor, BatchActor, is the identity of the combined update and is not judged.
//
// complete is false when the batch does not list all of its changes. The
// server bounds the size of the member list and keeps a prefix of it, so the
// last member's commit is then not the new tip; a batch may also list no
// members at all. The changes past the list are then represented by one more
// event for the new tip (unlistedPart), and the caller reports that the others
// were not inspected.
//
// Events that batchUnits derived are returned as they are.
func batchUnits(ev Event) (units []Event, complete bool) {
	if ev.part != wholeEvent || (ev.Operation != OperationLandBatch && len(ev.Members) == 0) {
		return []Event{ev}, true
	}

	units = make([]Event, 0, len(ev.Members)+1)
	prev := ev.OldOID
	for _, m := range ev.Members {
		u := ev
		u.Members = nil
		u.OldOID = prev
		u.NewOID = m.Commit
		u.Actor = m.Actor
		u.Rewritten = m.Rewritten
		u.part = memberPart
		units = append(units, u)
		prev = m.Commit
	}
	if len(ev.Members) > 0 && prev == ev.NewOID {
		return units, true
	}

	tip := ev
	tip.Members = nil
	tip.OldOID = prev
	tip.part = unlistedPart
	return append(units, tip), false
}

var oidPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ParseEvent decodes one JSON object and validates it. Unknown fields are
// ignored so that newer servers can add fields.
func ParseEvent(line []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return Event{}, fmt.Errorf("decode event: %w", err)
	}
	if ev.Sequence == 0 {
		return Event{}, errors.New("event has no sequence")
	}
	if !strings.HasPrefix(ev.Reference, "refs/") || len(ev.Reference) > 1024 || strings.ContainsAny(ev.Reference, "\x00\n") {
		return Event{}, fmt.Errorf("event %d: invalid reference", ev.Sequence)
	}
	if !oidPattern.MatchString(ev.NewOID) {
		return Event{}, fmt.Errorf("event %d: invalid new_oid", ev.Sequence)
	}
	if ev.OldOID != "" && !oidPattern.MatchString(ev.OldOID) {
		return Event{}, fmt.Errorf("event %d: invalid old_oid", ev.Sequence)
	}
	if ev.RecordedAt.IsZero() {
		return Event{}, fmt.Errorf("event %d: no recorded_at", ev.Sequence)
	}
	for i, m := range ev.Members {
		if !oidPattern.MatchString(m.Commit) {
			return Event{}, fmt.Errorf("event %d: member %d: invalid commit", ev.Sequence, i)
		}
		if m.OriginalCommit != "" && !oidPattern.MatchString(m.OriginalCommit) {
			return Event{}, fmt.Errorf("event %d: member %d: invalid original_commit", ev.Sequence, i)
		}
	}
	return ev, nil
}

// ReadEvents reads JSON lines from r and calls fn for each event in order. Blank
// lines are skipped. It stops at the first malformed line or the first error of fn.
func ReadEvents(r io.Reader, fn func(Event) error) error {
	src := NewReaderSource(r, nil)
	for {
		ev, err := src.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
}

// newScanner returns a line scanner bounded by MaxEventLine.
func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), MaxEventLine)
	return sc
}

// trimLine returns the trimmed content of a scanned line and whether it is blank.
func trimLine(b []byte) ([]byte, bool) {
	t := bytes.TrimSpace(b)
	return t, len(t) == 0
}
