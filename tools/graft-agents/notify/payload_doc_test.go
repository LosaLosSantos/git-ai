package notify

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// The webhook payload is documented field by field in doc/graft/SUPERFAN.md.
// This test pins the field names, so that a change to Message or Finding is a
// change to the documented contract and not a silent one.
func TestWebhookPayloadFieldsMatchDocumentation(t *testing.T) {
	msg := Message{
		Kind:        KindDigest,
		Component:   "security",
		WindowStart: time.Unix(0, 0),
		WindowEnd:   time.Unix(3600, 0),
		Events:      1,
		Risk:        "high",
		Summary:     "s",
		Omitted:     1,
		Findings: []Finding{{
			Rule: "protected-path", Severity: "high", Summary: "s", Sequence: 1,
			Reference: "refs/heads/main", Actor: "a", OID: "o", RecordedAt: time.Unix(1, 0),
		}},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	want := "events,findings,kind,omitted,risk,summary,window_end,window_start,component"
	if got := keys(top); got != sorted(want) {
		t.Errorf("message fields = %s, want %s", got, sorted(want))
	}
	var findings []map[string]json.RawMessage
	if err := json.Unmarshal(top["findings"], &findings); err != nil || len(findings) != 1 {
		t.Fatalf("findings = %s, %v", top["findings"], err)
	}
	want = "rule,severity,summary,sequence,reference,actor,oid,recorded_at"
	if got := keys(findings[0]); got != sorted(want) {
		t.Errorf("finding fields = %s, want %s", got, sorted(want))
	}
}

func keys(m map[string]json.RawMessage) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func sorted(list string) string {
	parts := strings.Split(list, ",")
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
