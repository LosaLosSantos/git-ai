package protocol

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestProtocolFieldsMatchTheServerCopy checks the wire fields of this copy of the protocol against
// the server's golden file (internal/graft/resolver/testdata/protocol-fields.json), which the
// server's copy is tested against too, so that the two copies cannot drift.
func TestProtocolFieldsMatchTheServerCopy(t *testing.T) {
	data, err := os.ReadFile("../../../internal/graft/resolver/testdata/protocol-fields.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]any
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	delete(golden, "comment")

	types := map[string]reflect.Type{
		"request":        reflect.TypeOf(Request{}),
		"repository":     reflect.TypeOf(Repository{}),
		"messages":       reflect.TypeOf(Messages{}),
		"file":           reflect.TypeOf(FileConflict{}),
		"side":           reflect.TypeOf(Side{}),
		"limits":         reflect.TypeOf(Limits{}),
		"response":       reflect.TypeOf(Response{}),
		"response_file":  reflect.TypeOf(ResolvedFile{}),
		"error_response": reflect.TypeOf(ErrorResponse{}),
		"file_outcome":   reflect.TypeOf(FileOutcome{}),
		"check_response": reflect.TypeOf(CheckResponse{}),
		"check_file":     reflect.TypeOf(CheckFile{}),
	}
	if len(golden) != len(types) {
		t.Fatalf("golden file has %d messages, this copy %d", len(golden), len(types))
	}
	for name, typ := range types {
		raw, ok := golden[name].([]any)
		if !ok {
			t.Fatalf("golden file lacks %q", name)
		}
		var want []string
		for _, v := range raw {
			want = append(want, v.(string))
		}
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			n, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if n != "" && n != "-" {
				got = append(got, n)
			}
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: fields %v, golden %v", name, got, want)
		}
	}
}
