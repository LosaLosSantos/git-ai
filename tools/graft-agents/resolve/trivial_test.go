package resolve

import (
	"context"
	"testing"

	"github.com/feder-cr/open_scion/tools/graft-agents/protocol"
)

func TestTrivialStrategy(t *testing.T) {
	base := sideOf(protocol.ModeFile, "a\nb\n")
	changedA := sideOf(protocol.ModeFile, "A\nb\n")
	changedB := sideOf(protocol.ModeFile, "a\nB\n")
	exec := sideOf(protocol.ModeExecutable, "a\nb\n")

	cases := []struct {
		name    string
		file    File
		ok      bool
		action  string
		content string
		mode    string
	}{
		{
			name:    "identical sides",
			file:    File{Base: base, Ours: changedA, Theirs: sideOf(protocol.ModeFile, "A\nb\n")},
			ok:      true,
			action:  protocol.ActionWrite,
			content: "A\nb\n",
			mode:    protocol.ModeFile,
		},
		{
			name:    "theirs unchanged takes ours",
			file:    File{Base: base, Ours: changedA, Theirs: base},
			ok:      true,
			action:  protocol.ActionWrite,
			content: "A\nb\n",
			mode:    protocol.ModeFile,
		},
		{
			name:    "ours unchanged takes theirs",
			file:    File{Base: base, Ours: base, Theirs: changedB},
			ok:      true,
			action:  protocol.ActionWrite,
			content: "a\nB\n",
			mode:    protocol.ModeFile,
		},
		{
			name:   "ours deleted, theirs unchanged deletes",
			file:   File{Base: base, Ours: nil, Theirs: base},
			ok:     true,
			action: protocol.ActionDelete,
		},
		{
			name:   "theirs deleted, ours unchanged deletes",
			file:   File{Base: base, Ours: base, Theirs: nil},
			ok:     true,
			action: protocol.ActionDelete,
		},
		{
			name:   "both deleted",
			file:   File{Base: base, Kind: protocol.KindModifyDelete},
			ok:     true,
			action: protocol.ActionDelete,
		},
		{
			name:    "added on one side only",
			file:    File{Base: nil, Ours: changedA, Theirs: nil},
			ok:      true,
			action:  protocol.ActionWrite,
			content: "A\nb\n",
			mode:    protocol.ModeFile,
		},
		{
			name:    "trailing whitespace difference keeps ours",
			file:    File{Base: base, Ours: changedA, Theirs: sideOf(protocol.ModeFile, "A \nb\t\n")},
			ok:      true,
			action:  protocol.ActionWrite,
			content: "A\nb\n",
			mode:    protocol.ModeFile,
		},
		{
			name:    "line ending difference keeps ours",
			file:    File{Base: nil, Ours: sideOf(protocol.ModeFile, "a \r\nb\n"), Theirs: sideOf(protocol.ModeFile, "a\nb\n")},
			ok:      true,
			action:  protocol.ActionWrite,
			content: "a \r\nb\n",
			mode:    protocol.ModeFile,
		},
		{
			name: "indentation change is not whitespace noise",
			file: File{
				Base:   sideOf(protocol.ModeFile, "x\ny\n"),
				Ours:   sideOf(protocol.ModeFile, "x \ny\n"),
				Theirs: sideOf(protocol.ModeFile, "x\n    y\n"),
			},
			ok: false,
		},
		{
			name: "binary file never takes the whitespace rule",
			file: File{
				Base:   sideOf(protocol.ModeFile, "\x00AB  CD"),
				Ours:   sideOf(protocol.ModeFile, "\x00AB \tCD"),
				Theirs: sideOf(protocol.ModeFile, "\x00AB\nCD"),
			},
			ok: false,
		},
		{
			name: "symlink never takes the whitespace rule",
			file: File{
				Base:   sideOf(protocol.ModeSymlink, "x"),
				Ours:   sideOf(protocol.ModeSymlink, "a b"),
				Theirs: sideOf(protocol.ModeSymlink, "a  b"),
			},
			ok: false,
		},
		{
			name: "real content conflict declines",
			file: File{Base: base, Ours: changedA, Theirs: changedB},
			ok:   false,
		},
		{
			name: "modify delete with change declines",
			file: File{Base: base, Ours: changedA, Theirs: nil},
			ok:   false,
		},
		{
			name: "mode differs and content differs declines",
			file: File{Base: base, Ours: exec, Theirs: changedB},
			ok:   false,
		},
		{
			name:    "mode only change on one side is taken",
			file:    File{Base: base, Ours: exec, Theirs: base},
			ok:      true,
			action:  protocol.ActionWrite,
			content: "a\nb\n",
			mode:    protocol.ModeExecutable,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, ok, err := trivial{}.Resolve(context.Background(), c.file, protocol.Request{})
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if res.Action != c.action {
				t.Fatalf("action = %q, want %q", res.Action, c.action)
			}
			if c.action == protocol.ActionWrite {
				if string(res.Content) != c.content || res.Mode != c.mode {
					t.Fatalf("answer = mode %q content %q, want mode %q content %q", res.Mode, res.Content, c.mode, c.content)
				}
			}
		})
	}
}

func TestTrivialFactoryRejectsOptions(t *testing.T) {
	if _, err := newTrivial(StrategySpec{Name: "t", Type: "trivial", Options: map[string]any{"x": 1}}); err == nil {
		t.Fatal("unknown option accepted")
	}
	if _, err := newTrivial(StrategySpec{Name: "t", Type: "trivial"}); err != nil {
		t.Fatalf("plain trivial rejected: %v", err)
	}
}

func TestTrimLineEnds(t *testing.T) {
	cases := map[string]string{
		" a \n\tb\t\r\n": " a\n\tb\n",
		"no change\n":    "no change\n",
		"x\r\n\r\n":      "x\n\n",
		"  leading kept": "  leading kept",
	}
	for in, want := range cases {
		if got := string(trimLineEnds([]byte(in))); got != want {
			t.Fatalf("trimLineEnds(%q) = %q, want %q", in, got, want)
		}
	}
}
