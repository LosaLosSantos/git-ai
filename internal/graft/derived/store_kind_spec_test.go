// Black-box spec tests for WP24 (KindStore / WarmEnv / warmer registry).
//
// Written from PLAN3.md (WP24 + RECONCILIATION) only.
//
// ASSUMPTIONS THAT MUST BE CHECKED AGAINST THE REAL CODE BEFORE RUNNING:
//   - gitalypb / git / log import paths below use module
//     gitlab.com/gitlab-org/gitaly/v16; adjust the module major version if the
//     repository uses another one.
//   - The spec does not name a Store constructor. specStore below calls
//     NewStore(dir string); if the real constructor differs, adjust specStore only.
package derived

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
)

// specStore returns a fresh, empty Store backed by a temp directory.
// Only this helper depends on a constructor the spec does not name.
func specStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

// specOID builds a 40-character object ID filled with c. Values only need to be
// distinct; they are not required to be reachable commits.
func specOID(c byte) git.ObjectID {
	return git.ObjectID(strings.Repeat(string(c), 40))
}

// specSuffix returns a unique suffix. Digits are mapped to letters because
// warmer and kind names follow the store's kind syntax ([a-z][a-z-]*).
func specSuffix() string {
	return strings.Map(func(r rune) rune { return 'a' + (r - '0') }, fmt.Sprintf("%d", time.Now().UnixNano()))
}

func TestSpecKindStorePutThenGet(t *testing.T) {
	k := specStore(t).Kind("depth", 1)

	if err := k.Put(specOID('a'), []byte("v1")); err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	got, ok, err := k.Get(specOID('a'))
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if !ok {
		t.Fatalf("Get after Put: ok = false, want true")
	}
	if !bytes.Equal(got, []byte("v1")) {
		t.Fatalf("Get after Put: value = %q, want %q", got, "v1")
	}
}

func TestSpecKindStoreMissingCommitIsNotAnError(t *testing.T) {
	k := specStore(t).Kind("depth", 1)

	got, ok, err := k.Get(specOID('b'))
	if err != nil {
		t.Fatalf("Get of a never-stored commit returned error %v, want nil", err)
	}
	if ok {
		t.Fatalf("Get of a never-stored commit: ok = true, want false")
	}
	if len(got) != 0 {
		t.Fatalf("Get of a never-stored commit returned value %q, want none", got)
	}
}

func TestSpecKindStoreVersionBumpInvalidates(t *testing.T) {
	s := specStore(t)

	if err := s.Kind("depth", 1).Put(specOID('c'), []byte("old")); err != nil {
		t.Fatalf("Put (version 1) returned error: %v", err)
	}

	_, ok, err := s.Kind("depth", 2).Get(specOID('c'))
	if err != nil {
		t.Fatalf("Get (version 2) returned error: %v", err)
	}
	if ok {
		t.Fatalf("value stored under version 1 is visible under version 2; a version bump must invalidate stored values")
	}
}

func TestSpecKindStoreKindsAreIsolatedByName(t *testing.T) {
	s := specStore(t)

	if err := s.Kind("graph", 1).Put(specOID('d'), []byte("graph-value")); err != nil {
		t.Fatalf("Put (graph) returned error: %v", err)
	}

	_, ok, err := s.Kind("changed-paths", 1).Get(specOID('d'))
	if err != nil {
		t.Fatalf("Get (changed-paths) returned error: %v", err)
	}
	if ok {
		t.Fatalf("value of kind %q leaked into kind %q for the same commit", "graph", "changed-paths")
	}
}

func TestSpecKindStoreViewsShareData(t *testing.T) {
	s := specStore(t)

	if err := s.Kind("depth", 1).Put(specOID('e'), []byte("shared")); err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	// A second KindStore view for the same name and version must see the value.
	got, ok, err := s.Kind("depth", 1).Get(specOID('e'))
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if !ok || !bytes.Equal(got, []byte("shared")) {
		t.Fatalf("second Kind view did not see the stored value: ok=%v value=%q", ok, got)
	}
}

func TestSpecKindStoreBinaryValuesRoundTrip(t *testing.T) {
	k := specStore(t).Kind("graph", 1)

	// Graph values are binary: little-endian uint32, uint8, raw object IDs.
	// NUL bytes and high bits must survive unchanged.
	want := []byte{0x00, 0x01, 0x00, 0x00, 0x02, 0xff, 0x00, 0x80, 0x7f}
	if err := k.Put(specOID('f'), want); err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	got, ok, err := k.Get(specOID('f'))
	if err != nil || !ok {
		t.Fatalf("Get returned ok=%v err=%v, want ok=true err=nil", ok, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("binary value changed on round trip: got %x, want %x", got, want)
	}
}

func TestSpecRegisterWarmerListsSortedNames(t *testing.T) {
	sfx := specSuffix()
	noop := func(context.Context, WarmEnv, []git.ObjectID) error { return nil }

	RegisterWarmer("spec-zeta-"+sfx, noop)
	RegisterWarmer("spec-alpha-"+sfx, noop)

	names := Warmers()
	if !sort.StringsAreSorted(names) {
		t.Fatalf("Warmers() is not sorted: %v", names)
	}

	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	for _, want := range []string{"spec-zeta-" + sfx, "spec-alpha-" + sfx} {
		if !found[want] {
			t.Fatalf("Warmers() = %v does not contain registered warmer %q", names, want)
		}
	}
}
