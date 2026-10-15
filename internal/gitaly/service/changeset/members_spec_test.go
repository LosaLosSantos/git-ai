package changeset

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	// Module path assumed to be the Gitaly module root; adjust if the module differs.
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// WP23 / Foundation: message ChangesetMember { string original_commit_id = 1;
// string commit_id = 2; string actor = 3; bool rewritten = 4;
// repeated bytes resolved_paths = 5; }
func TestSpecChangesetMemberProtoFields(t *testing.T) {
	fields := (&gitalypb.ChangesetMember{}).ProtoReflect().Descriptor().Fields()

	want := map[string]protoreflect.FieldNumber{
		"original_commit_id": 1,
		"commit_id":          2,
		"actor":              3,
		"rewritten":          4,
		"resolved_paths":     5,
	}

	for name, number := range want {
		f := fields.ByName(protoreflect.Name(name))
		if f == nil {
			t.Errorf("ChangesetMember has no field %q", name)
			continue
		}
		if f.Number() != number {
			t.Errorf("ChangesetMember.%s has field number %d, want %d", name, f.Number(), number)
		}
	}

	if fields.Len() != len(want) {
		t.Errorf("ChangesetMember has %d fields, want exactly %d", fields.Len(), len(want))
	}

	if f := fields.ByName("resolved_paths"); f != nil && (!f.IsList() || f.Kind() != protoreflect.BytesKind) {
		t.Errorf("ChangesetMember.resolved_paths must be repeated bytes")
	}
}

// WP23 / Foundation: Changeset.members is a repeated ChangesetMember at field 9.
func TestSpecChangesetMembersFieldShape(t *testing.T) {
	f := (&gitalypb.Changeset{}).ProtoReflect().Descriptor().Fields().ByName("members")
	if f == nil {
		t.Fatalf("Changeset has no \"members\" field")
	}

	if f.Number() != 9 {
		t.Errorf("Changeset.members has field number %d, want 9", f.Number())
	}
	if !f.IsList() {
		t.Errorf("Changeset.members must be repeated")
	}
	if f.Message() == nil || f.Message().Name() != "ChangesetMember" {
		t.Errorf("Changeset.members must be a repeated ChangesetMember")
	}
}

// WP23: members, including non-UTF-8 resolved path bytes, survive a protobuf
// round trip on a Changeset.
func TestSpecChangesetMembersProtoRoundTrip(t *testing.T) {
	in := &gitalypb.Changeset{
		Members: []*gitalypb.ChangesetMember{
			{
				OriginalCommitId: "aaaa",
				CommitId:         "bbbb",
				Actor:            "agent-1",
				Rewritten:        true,
				ResolvedPaths:    [][]byte{[]byte("dir/file.txt"), {0xff, 0xfe, 'x'}},
			},
			{
				OriginalCommitId: "cccc",
				CommitId:         "dddd",
				Actor:            "agent-2",
			},
		},
	}

	raw, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("proto.Marshal(Changeset) failed: %v", err)
	}

	out := &gitalypb.Changeset{}
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatalf("proto.Unmarshal(Changeset) failed: %v", err)
	}

	if !proto.Equal(in, out) {
		t.Errorf("Changeset members changed across a proto round trip:\n got  %v\n want %v", out, in)
	}

	if len(out.GetMembers()) != 2 {
		t.Fatalf("round-tripped Changeset has %d members, want 2", len(out.GetMembers()))
	}
	if got := out.GetMembers()[0].GetResolvedPaths()[1]; string(got) != "\xff\xfex" {
		t.Errorf("non-UTF-8 resolved path bytes were not preserved: %x", got)
	}
}
