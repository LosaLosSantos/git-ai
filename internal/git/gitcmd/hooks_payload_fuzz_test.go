package gitcmd

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"unicode/utf8"
)

// FuzzChangelogMetaFromEnv fuzzes the decoding of the changeset log metadata
// that the hooks payload carries in the environment. Arbitrary values must not
// panic, and metadata encoded the way NewHooksPayload encodes it must decode to
// the same fields.
func FuzzChangelogMetaFromEnv(f *testing.F) {
	f.Add("", "", "", false, false)
	f.Add("not base64!", "ssh_receive_pack", "user-1", true, false)
	f.Add(base64.StdEncoding.EncodeToString([]byte(`{"repository":"x"}`)), "land_batch", "agent-7", false, true)
	f.Add(base64.StdEncoding.EncodeToString([]byte(`{"graft_changelog":{"operation":"op","members_truncated":true}}`)), "", "", false, false)
	f.Add(base64.StdEncoding.EncodeToString([]byte(`{"graft_changelog":null}`)), "", "", false, false)
	f.Add(base64.StdEncoding.EncodeToString([]byte(`[1,2,3]`)), "", "", false, false)
	f.Add(base64.StdEncoding.EncodeToString([]byte(`{"graft_changelog":{"members":[{"x":1}]}}`)), "", "", false, false)

	f.Fuzz(func(t *testing.T, value, operation, actor string, agent, rewritten bool) {
		// Arbitrary values, as the environment of a git command could hold them.
		for _, env := range [][]string{
			{EnvHooksPayload + "=" + value},
			{"OTHER=1", EnvHooksPayload + "=" + value, EnvHooksPayload + "=second"},
			{EnvHooksPayload},
		} {
			first, err1 := ChangelogMetaFromEnv(env)
			second, err2 := ChangelogMetaFromEnv(env)

			if (err1 == nil) != (err2 == nil) {
				t.Fatalf("non-deterministic error: %v != %v", err1, err2)
			}

			// Every decoded field, not just one, must be the same on both calls.
			if err1 == nil && !reflect.DeepEqual(first, second) {
				t.Fatalf("non-deterministic result: %+v != %+v", first, second)
			}
		}

		// Metadata that was encoded must decode to the same fields. encoding/json
		// replaces invalid UTF-8, so only valid strings are compared.
		if !utf8.ValidString(operation) || !utf8.ValidString(actor) {
			return
		}

		meta := ChangelogMeta{
			Operation:  operation,
			Actor:      actor,
			Agent:      agent,
			Rewritten:  rewritten,
			Invocation: "0123456789abcdef",
		}

		encoded, err := json.Marshal(jsonHooksPayload{Changelog: &meta})
		if err != nil {
			t.Fatalf("encoding the payload: %v", err)
		}

		decoded, err := ChangelogMetaFromEnv([]string{EnvHooksPayload + "=" + base64.StdEncoding.EncodeToString(encoded)})
		if err != nil {
			t.Fatalf("decoding the payload %s: %v", encoded, err)
		}

		if decoded.Operation != meta.Operation || decoded.Actor != meta.Actor ||
			decoded.Agent != meta.Agent || decoded.Rewritten != meta.Rewritten ||
			decoded.Invocation != meta.Invocation {
			t.Fatalf("metadata changed in round trip: %+v != %+v", decoded, meta)
		}
	})
}
