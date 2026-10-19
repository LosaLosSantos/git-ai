package bench

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestNotSentReasonVectors checks notSentReason against the server's test vectors
// (internal/graft/resolver/testdata/never-send.json), so that the bench and the server apply the
// same never-send rules in the same order.
func TestNotSentReasonVectors(t *testing.T) {
	data, err := os.ReadFile("../../../../internal/graft/resolver/testdata/never-send.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		MaxFileBytes int64 `json:"max_file_bytes"`
		Cases        []struct {
			Name      string `json:"name"`
			Path      string `json:"path"`
			Generated bool   `json:"generated"`
			Sides     []*[]struct {
				Text   string `json:"text"`
				Repeat int    `json:"repeat"`
			} `json:"sides"`
			Want string `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range v.Cases {
		var sides [][]byte
		for _, side := range c.Sides {
			if side == nil {
				continue // the bench passes present sides only
			}
			var b strings.Builder
			for _, part := range *side {
				b.WriteString(strings.Repeat(part.Text, max(1, part.Repeat)))
			}
			sides = append(sides, []byte(b.String()))
		}
		if got := notSentReason(c.Path, sides, v.MaxFileBytes, c.Generated); got != c.Want {
			t.Errorf("%s: notSentReason = %q, want %q", c.Name, got, c.Want)
		}
	}
}
