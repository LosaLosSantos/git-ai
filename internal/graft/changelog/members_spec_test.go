package changelog

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// specMembers returns sample members as described by WP23: Member has
// OriginalCommit, Commit, Actor, Rewritten and ResolvedPaths.
func specMembers() []Member {
	return []Member{
		{
			OriginalCommit: "1111111111111111111111111111111111111111",
			Commit:         "2222222222222222222222222222222222222222",
			Actor:          "agent-1",
			Rewritten:      false,
			ResolvedPaths:  nil,
		},
		{
			OriginalCommit: "3333333333333333333333333333333333333333",
			Commit:         "4444444444444444444444444444444444444444",
			Actor:          "agent-2",
			Rewritten:      true,
			ResolvedPaths:  []string{"dir/ünï.txt", "with space.md"},
		},
	}
}

func specDecodeObject(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%T) failed: %v", v, err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("encoded %T is not a JSON object (%s): %v", v, raw, err)
	}

	return obj
}

// WP23: Member JSON keys are exactly original_commit, commit, actor,
// rewritten, resolved_paths.
func TestSpecMemberJSONKeys(t *testing.T) {
	obj := specDecodeObject(t, Member{
		OriginalCommit: "aa",
		Commit:         "bb",
		Actor:          "cc",
		Rewritten:      true,
		ResolvedPaths:  []string{"p/q"},
	})

	want := map[string]string{
		"original_commit": `"aa"`,
		"commit":          `"bb"`,
		"actor":           `"cc"`,
		"rewritten":       `true`,
		"resolved_paths":  `["p/q"]`,
	}

	for key, value := range want {
		got, ok := obj[key]
		if !ok {
			t.Errorf("Member JSON is missing key %q", key)
			continue
		}
		if string(got) != value {
			t.Errorf("Member JSON key %q = %s, want %s", key, got, value)
		}
	}

	if len(obj) != len(want) {
		t.Errorf("Member JSON has %d keys, want exactly %d", len(obj), len(want))
	}
}

// WP23: members survive a JSON round trip unchanged (including UTF-8 and
// space-containing paths).
func TestSpecMemberJSONRoundTrip(t *testing.T) {
	in := specMembers()

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("json.Marshal([]Member) failed: %v", err)
	}

	var out []Member
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json.Unmarshal of %s failed: %v", raw, err)
	}

	if !reflect.DeepEqual(in, out) {
		t.Errorf("Member JSON round trip changed the data:\n got  %+v\n want %+v\n json %s", out, in, raw)
	}
}

// WP23: Entry.Members is `json:"members,omitempty"`, so an entry without
// members must not carry the key.
func TestSpecEntryMembersOmittedWhenEmpty(t *testing.T) {
	obj := specDecodeObject(t, Entry{})

	if _, ok := obj["members"]; ok {
		t.Errorf("Entry with no members encoded a \"members\" key; it must be omitted")
	}
}

// WP23: a non-empty Entry.Members is encoded under the key "members" as an
// array of member objects.
func TestSpecEntryMembersEncoded(t *testing.T) {
	obj := specDecodeObject(t, Entry{Members: specMembers()})

	raw, ok := obj["members"]
	if !ok {
		t.Fatalf("Entry with members did not encode a \"members\" key")
	}

	var got []Member
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("\"members\" is not an array of Member (%s): %v", raw, err)
	}

	if !reflect.DeepEqual(got, specMembers()) {
		t.Errorf("encoded entry members = %+v, want %+v", got, specMembers())
	}
}

// WP23: Meta carries Members and Operation (the hook checks
// Operation == "land_batch" together with non-empty Members).
func TestSpecMetaCarriesMembersAndOperation(t *testing.T) {
	m := Meta{Operation: "land_batch", Members: specMembers()}

	if m.Operation != "land_batch" {
		t.Errorf("Meta.Operation = %q, want %q", m.Operation, "land_batch")
	}
	if !reflect.DeepEqual(m.Members, specMembers()) {
		t.Errorf("Meta.Members = %+v, want %+v", m.Members, specMembers())
	}
}

// WP23: WithBatch / BatchFromContext round trip the exact member list.
func TestSpecWithBatchRoundTrip(t *testing.T) {
	in := specMembers()

	ctx := WithBatch(context.Background(), in)
	got := BatchFromContext(ctx)

	if !reflect.DeepEqual(got, in) {
		t.Errorf("BatchFromContext(WithBatch(ctx, members)) = %+v, want %+v", got, in)
	}
}

// WP23: BatchFromContext on a context without members yields no members.
func TestSpecBatchFromContextAbsent(t *testing.T) {
	if got := BatchFromContext(context.Background()); len(got) != 0 {
		t.Errorf("BatchFromContext(background) = %+v, want no members", got)
	}
}

// WP23: members are attached to the context they were added to, not to
// its parent.
func TestSpecWithBatchIsScopedToChildContext(t *testing.T) {
	parent := context.Background()
	_ = WithBatch(parent, specMembers())

	if got := BatchFromContext(parent); len(got) != 0 {
		t.Errorf("parent context sees members added to a child context: %+v", got)
	}
}
