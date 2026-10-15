package resolve

import "testing"

func TestParseTOMLTableDefinedAfterSubtable(t *testing.T) {
	// TOML allows [a.b] followed by [a]: the table a exists implicitly first.
	got, err := parseTOML("[a.b]\nx = 1\n[a]\ny = 2\n")
	if err != nil {
		t.Fatalf("parseTOML: %v", err)
	}
	a, ok := got["a"].(map[string]any)
	if !ok || a["y"] != int64(2) {
		t.Fatalf("a = %#v", got["a"])
	}
	if b, ok := a["b"].(map[string]any); !ok || b["x"] != int64(1) {
		t.Fatalf("a.b = %#v", a["b"])
	}
}

func TestParseTOMLRejectsRedefinedTables(t *testing.T) {
	for _, src := range []string{
		"[a]\n[a.b]\n[a]",
		"[a.b]\n[a.b]",
		"[a.b]\n[a]\n[a]",
	} {
		if _, err := parseTOML(src); err == nil {
			t.Fatalf("parseTOML(%q) accepted a redefined table", src)
		}
	}
}

func TestParseTOMLSubtablePerArrayElement(t *testing.T) {
	// Each element of an array of tables may have its own sub-table.
	src := "[[s]]\ntype = \"x\"\n[s.llm]\nm = 1\n[[s]]\ntype = \"y\"\n[s.llm]\nm = 2\n"
	got, err := parseTOML(src)
	if err != nil {
		t.Fatalf("parseTOML: %v", err)
	}
	elems, ok := got["s"].([]map[string]any)
	if !ok || len(elems) != 2 {
		t.Fatalf("s = %#v", got["s"])
	}
	for i, want := range []int64{1, 2} {
		llm, ok := elems[i]["llm"].(map[string]any)
		if !ok || llm["m"] != want {
			t.Fatalf("element %d llm = %#v", i, elems[i]["llm"])
		}
	}
}
