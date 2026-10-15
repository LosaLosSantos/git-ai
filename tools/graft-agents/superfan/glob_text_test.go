package superfan

import (
	"math/rand"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// refMatch is the backtracking matcher that matchSegments replaces. It is
// exponential in the number of "**" segments, so it serves only as the oracle
// of the small patterns below.
func refMatch(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if refMatch(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	return segmentMatch(pat[0], name[0]) && refMatch(pat[1:], name[1:])
}

func randomSegments(rng *rand.Rand, alphabet []string, maxLen int) []string {
	out := make([]string, rng.Intn(maxLen+1))
	for i := range out {
		out[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return out
}

func TestMatchSegmentsAgreesWithBacktracking(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	patterns := []string{"a", "b", "*", "**", "?"}
	names := []string{"a", "b", "ab"}
	for i := 0; i < 20000; i++ {
		pat := randomSegments(rng, patterns, 5)
		name := randomSegments(rng, names, 6)
		if got, want := matchSegments(pat, name), refMatch(pat, name); got != want {
			t.Fatalf("matchSegments(%q, %q) = %v, backtracking says %v", pat, name, got, want)
		}
	}
}

func TestMatchPathStaysCheapWithManyDoubleStars(t *testing.T) {
	// Four "**" segments and a deep path: backtracking took seconds here.
	pattern := "**/*/**/*/**/*/**/zz"
	if err := ValidGlob(pattern); err != nil {
		t.Fatal(err)
	}
	name := strings.TrimSuffix(strings.Repeat("a/", 2000), "/")
	start := time.Now()
	if MatchPath(pattern, name) {
		t.Fatal("matched a path without zz")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("matching took %s", elapsed)
	}
	if !MatchPath("**/*/**/*/**/*/**/zz", name+"/zz") {
		t.Error("did not match a path that ends with zz")
	}
}

func TestOneLineStripsControlsAndBounds(t *testing.T) {
	if got := oneLine("a\x1b[2Jb\nc", 100); got != "a [2Jb c" {
		t.Errorf("oneLine = %q", got)
	}
	if got := multiLine("a\nb\x1b", 100); got != "a\nb " {
		t.Errorf("multiLine = %q", got)
	}
	if got := oneLine(strings.Repeat("é", 1000), 11); len(got) > 11 || !utf8.ValidString(got) {
		t.Errorf("oneLine cut = %q", got)
	}
	if got := oneLine(strings.Repeat("x", 10<<20), 512); len(got) != 512 {
		t.Errorf("oneLine of a huge input = %d bytes, want 512", len(got))
	}
}
