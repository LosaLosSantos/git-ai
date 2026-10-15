package sdk

import (
	"testing"

	"google.golang.org/protobuf/proto"

	// Module path assumed to be the Gitaly module root; adjust if the module differs.
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// WP23: the SDK exposes changeset members through its Changeset type with no
// extra mapping. Verified by requiring Changeset.Members to be the proto
// member slice type (true if Changeset is the proto alias).
func TestSpecSDKChangesetExposesMembers(t *testing.T) {
	var c Changeset
	c.Members = []*gitalypb.ChangesetMember{
		{OriginalCommitId: "aa", CommitId: "bb", Actor: "agent-1", Rewritten: true},
	}

	if len(c.Members) != 1 || c.Members[0].GetActor() != "agent-1" {
		t.Fatalf("SDK Changeset did not keep members: %+v", c.Members)
	}
}

// WP23: members of an SDK Changeset survive the protobuf encoding used on the
// wire, so the SDK sees the same members the service sent.
func TestSpecSDKChangesetMembersSurviveProto(t *testing.T) {
	in := &Changeset{
		Members: []*gitalypb.ChangesetMember{
			{OriginalCommitId: "aa", CommitId: "bb", Actor: "agent-1", ResolvedPaths: [][]byte{[]byte("a/b.txt")}},
		},
	}

	raw, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("proto.Marshal(Changeset) failed: %v", err)
	}

	out := &Changeset{}
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatalf("proto.Unmarshal(Changeset) failed: %v", err)
	}

	if !proto.Equal(in, out) {
		t.Errorf("SDK Changeset members changed across proto encoding:\n got  %v\n want %v", out, in)
	}
}
