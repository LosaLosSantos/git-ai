package resolver

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// jsonFields returns the sorted JSON field names of a struct type.
func jsonFields(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestProtocolFieldsMatchTheOtherCopy checks the wire fields of this copy of the protocol against
// testdata/protocol-fields.json, which tools/graft-agents/protocol is tested against too.
func TestProtocolFieldsMatchTheOtherCopy(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/protocol-fields.json")
	require.NoError(t, err)
	var golden map[string]any
	require.NoError(t, json.Unmarshal(data, &golden))
	delete(golden, "comment")

	types := map[string]reflect.Type{
		"request":        reflect.TypeOf(Request{}),
		"repository":     reflect.TypeOf(Repository{}),
		"messages":       reflect.TypeOf(Messages{}),
		"file":           reflect.TypeOf(File{}),
		"side":           reflect.TypeOf(sideJSON{}), // Side's wire form
		"limits":         reflect.TypeOf(Limits{}),
		"response":       reflect.TypeOf(Response{}),
		"response_file":  reflect.TypeOf(ResponseFile{}),
		"error_response": reflect.TypeOf(ErrorResponse{}),
		"file_outcome":   reflect.TypeOf(FileOutcome{}),
		"check_response": reflect.TypeOf(CheckResponse{}),
		"check_file":     reflect.TypeOf(CheckFile{}),
	}
	require.Len(t, golden, len(types), "every message of the golden file has a type here")
	for name, typ := range types {
		raw, ok := golden[name].([]any)
		require.True(t, ok, "golden file lacks %q", name)
		var want []string
		for _, v := range raw {
			want = append(want, v.(string))
		}
		require.Equal(t, want, jsonFields(typ), name)
	}
}
