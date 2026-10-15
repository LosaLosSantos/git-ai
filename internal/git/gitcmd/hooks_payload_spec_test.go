package gitcmd

import (
	"reflect"
	"testing"

	// Module path assumed to be the Gitaly module root; adjust if the module differs.
	"github.com/feder-cr/open_scion/internal/graft/changelog"
)

func specMemberList() []changelog.Member {
	return []changelog.Member{
		{OriginalCommit: "aa", Commit: "bb", Actor: "agent-1"},
	}
}

// WP23: ChangelogMeta.IsZero reports true for the zero value.
func TestSpecChangelogMetaIsZeroForZeroValue(t *testing.T) {
	if !(ChangelogMeta{}).IsZero() {
		t.Errorf("ChangelogMeta{}.IsZero() = false, want true")
	}
}

// WP23: a ChangelogMeta carrying members is not zero.
func TestSpecChangelogMetaIsZeroFalseWithMembers(t *testing.T) {
	m := ChangelogMeta{Members: specMemberList()}

	if m.IsZero() {
		t.Errorf("ChangelogMeta with members reported IsZero() = true, want false")
	}
}

// WP23: ChangelogMeta stops being comparable (it now carries a slice), so
// comparisons must go through IsZero.
func TestSpecChangelogMetaIsNotComparable(t *testing.T) {
	if reflect.TypeOf(ChangelogMeta{}).Comparable() {
		t.Errorf("ChangelogMeta is comparable; it must not be once it carries Members")
	}
}

// WP23: ChangelogMeta exposes MembersTruncated (set when the encoded members
// exceed the 32 KiB cap).
func TestSpecChangelogMetaMembersTruncatedField(t *testing.T) {
	m := ChangelogMeta{MembersTruncated: true}

	if !m.MembersTruncated {
		t.Errorf("ChangelogMeta.MembersTruncated did not hold the assigned value")
	}
}
