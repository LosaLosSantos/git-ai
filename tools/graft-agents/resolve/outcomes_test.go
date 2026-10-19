package resolve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/tools/graft-agents/protocol"
)

// TestUnresolvedErrorResponseHasEveryFile: a refused request answers the outcome of every file:
// resolved, rejected (an answer failed a rule or a check) or failed (no strategy applied).
func TestUnresolvedErrorResponseHasEveryFile(t *testing.T) {
	cfg, err := ParseConfig([]byte("[[strategy]]\ntype = \"trivial\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	side := func(c string) *protocol.Side {
		return &protocol.Side{OID: "1", Mode: protocol.ModeFile, Content: []byte(c)}
	}
	req := protocol.Request{
		Version: protocol.Version,
		Files: []protocol.FileConflict{
			// trivial resolves a file that only one side changed.
			{Path: "one-side.txt", Kind: protocol.KindContent, Base: side("1\n"), Ours: side("1\n"), Theirs: side("2\n")},
			{Path: "both.txt", Kind: protocol.KindContent, Base: side("1\n"), Ours: side("2\n"), Theirs: side("3\n")},
		},
		Limits: protocol.Limits{MaxFileBytes: 1 << 20},
	}
	_, err = r.Resolve(context.Background(), req)
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("err = %v", err)
	}
	resp := unresolved.Response()
	if resp.Error == "" || len(resp.Files) != 2 {
		t.Fatalf("response %+v", resp)
	}
	got := map[string]protocol.FileOutcome{}
	for _, f := range resp.Files {
		got[f.Path] = f
	}
	if got["one-side.txt"].Outcome != protocol.OutcomeResolved {
		t.Errorf("one-side.txt: %+v, want resolved", got["one-side.txt"])
	}
	if f := got["both.txt"]; f.Outcome != protocol.OutcomeFailed || !strings.Contains(f.Reason, "no strategy") {
		t.Errorf("both.txt: %+v, want failed with the reason", f)
	}
}

func TestUnresolvedErrorResponseRejectedAndCut(t *testing.T) {
	e := &UnresolvedError{Failures: []Failure{{
		Path:     "a.go",
		Reasons:  []string{strings.Repeat("x", 600)},
		Attempts: []Attempt{{Strategy: "llm", Kind: AttemptCheckFailed, Err: errors.New("syntax")}},
	}}}
	f := e.Response().Files[0]
	if f.Outcome != protocol.OutcomeRejected || len(f.Reason) != maxOutcomeReasonBytes {
		t.Fatalf("outcome %+v (reason %d bytes)", f, len(f.Reason))
	}
}
