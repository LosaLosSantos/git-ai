package protocol

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestEnumValuesMatchTheServerCopy pins the values of the enumerations of protocol v1 to the
// server's golden file (internal/graft/resolver/testdata/protocol-enums.json), which the server's
// copy is tested against too: a value that changes on one side only would make the two programs
// misread each other.
func TestEnumValuesMatchTheServerCopy(t *testing.T) {
	data, err := os.ReadFile("../../../internal/graft/resolver/testdata/protocol-enums.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]any
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if v, _ := golden["version"].(float64); int(v) != Version {
		t.Fatalf("version = %v, golden %v", Version, golden["version"])
	}
	got := map[string][]string{
		"kind":       {KindContent, KindModifyDelete, KindAddAdd, KindMode},
		"action":     {ActionWrite, ActionDelete},
		"mode":       {ModeFile, ModeExecutable, ModeSymlink},
		"operation":  {OperationResolve, OperationCheck},
		"syntax":     {SyntaxChecked, SyntaxUnchecked, SyntaxNone},
		"origin":     {OriginRule, OriginModel},
		"outcome":    {OutcomeResolved, OutcomeRejected, OutcomeFailed},
		"capability": {CapabilityOrigin, CapabilityProposal, CapabilityHold, CapabilityVerify},
	}
	for name, want := range got {
		raw, ok := golden[name].([]any)
		if !ok {
			t.Fatalf("golden file lacks %q", name)
		}
		var values []string
		for _, v := range raw {
			values = append(values, v.(string))
		}
		if !reflect.DeepEqual(values, want) {
			t.Errorf("%s = %v, golden %v", name, want, values)
		}
	}
	if len(golden) != len(got)+2 {
		t.Errorf("the golden file holds %d entries, want a comment, the version and %d enumerations", len(golden), len(got))
	}
}
