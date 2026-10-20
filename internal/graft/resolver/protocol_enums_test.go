package resolver

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProtocolEnumValuesMatchTheGolden pins the values of the enumerations of protocol v1 to
// testdata/protocol-enums.json, which tools/graft-agents/protocol is tested against too: a value
// that changes on one side only would make the two programs misread each other.
func TestProtocolEnumValuesMatchTheGolden(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/protocol-enums.json")
	require.NoError(t, err)
	var golden map[string]any
	require.NoError(t, json.Unmarshal(data, &golden))

	require.EqualValues(t, ProtocolVersion, golden["version"])
	got := map[string][]string{
		"kind":      {KindContent, KindModifyDelete, KindAddAdd, KindMode},
		"action":    {ActionWrite, ActionDelete},
		"mode":      {ModeRegular, ModeExecutable, ModeSymlink},
		"operation": {OperationResolve, OperationCheck},
		"syntax":    {SyntaxChecked, SyntaxUnchecked, SyntaxNone},
		"origin":    {OriginRule, OriginModel},
		"outcome":   {OutcomeResolved, OutcomeRejected, OutcomeFailed},
	}
	for name, want := range got {
		raw, ok := golden[name].([]any)
		require.True(t, ok, "the golden file lacks %q", name)
		var values []string
		for _, v := range raw {
			values = append(values, v.(string))
		}
		require.Equal(t, values, want, name)
	}
	// Every enumeration of the golden file is checked here.
	require.Len(t, golden, len(got)+2, "the golden file holds a comment, the version and the enumerations")
}
