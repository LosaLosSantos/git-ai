package graft

// Black-box spec tests for WP29 (docs part). They read the documentation
// files from the spec only. When merged into the repository this file lives in
// doc/graft/, so the default paths resolve. Override with GRAFT_DOC_DIR (the
// directory holding the docs) and GRAFT_REPO_ROOT (the repository root, for
// README.md) when running from elsewhere.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func specDocDir() string {
	if d := os.Getenv("GRAFT_DOC_DIR"); d != "" {
		return d
	}
	return "."
}

func specReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return string(b)
}

func specDoc(t *testing.T, name string) string {
	t.Helper()
	return specReadFile(t, filepath.Join(specDocDir(), name))
}

// specLine reports whether some single line of text matches pattern (RE2 syntax).
func specLine(text, pattern string) bool {
	return regexp.MustCompile(`(?im)` + pattern).MatchString(text)
}

func specRequireContains(t *testing.T, file, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("%s must mention %q (spec requires it), but it does not", file, w)
		}
	}
}

// Spec: [graft.land.batch] disabled/window/max_size; window default 10ms,
// max_size default 50.
func TestSpecConfigLandBatchDefaults(t *testing.T) {
	const file = "CONFIGURATION.md"
	text := specDoc(t, file)

	specRequireContains(t, file, text, "[graft.land.batch]", "disabled", "window", "max_size")

	if !specLine(text, `window.*10ms`) {
		t.Errorf("%s: the line documenting `window` must state the default 10ms", file)
	}
	if !specLine(text, `max_size.*\b50\b`) {
		t.Errorf("%s: the line documenting `max_size` must state the default 50", file)
	}
}

// Spec: [graft.derived] keys, Workers default 2, QueueSize default 10000,
// Eager and Disabled switches.
func TestSpecConfigDerivedDefaults(t *testing.T) {
	const file = "CONFIGURATION.md"
	text := specDoc(t, file)

	specRequireContains(t, file, text, "[graft.derived]", "workers", "queue_size", "eager", "disabled")

	if !specLine(text, `workers.*\b2\b`) {
		t.Errorf("%s: the line documenting `workers` must state the default 2", file)
	}
	if !specLine(text, `queue_size.*(10000|10_000)`) {
		t.Errorf("%s: the line documenting `queue_size` must state the default 10000", file)
	}
}

// Spec: batching only for standalone servers and not MVCC; the synthetic
// graft-batch user is used for the combined update; batch_size and
// batch_position are reported on the land response.
func TestSpecLandBatchingDocCoversSemantics(t *testing.T) {
	const file = "LAND-BATCHING.md"
	text := specDoc(t, file)

	specRequireContains(t, file, text,
		"[graft.land.batch]",
		"standalone",
		"batch_size",
		"batch_position",
		"graft-batch",
		"resolve_conflicts",
	)
}

// Spec: CLI output `landed <oid> (batch 3/17, 1 attempt)`; the docs must show
// that shape so they match the code.
func TestSpecLandBatchingDocDescribesCLIOutput(t *testing.T) {
	const file = "LAND-BATCHING.md"
	text := specDoc(t, file)

	if !strings.Contains(text, "landed") {
		t.Errorf("%s must show the `landed <oid> (...)` output line", file)
	}
	if !specLine(text, `landed.*\(batch \d+/\d+, \d+ attempts?\)`) {
		t.Errorf("%s must show the CLI output shape `landed <oid> (batch N/M, K attempt(s))`", file)
	}
}

// Spec: the batching disabled behaviour is the old unbatched path and
// responses then report batch_size=1 and batch_position=1.
func TestSpecLandBatchingDocDisabledBehaviour(t *testing.T) {
	const file = "LAND-BATCHING.md"
	text := specDoc(t, file)

	if !specLine(text, `disabled`) {
		t.Errorf("%s must describe the disabled ([graft.land.batch] disabled = true) behaviour", file)
	}
	if !specLine(text, `(batch_size|batch size).*\b1\b|\b1\b.*(batch_size|batch size)`) {
		t.Errorf("%s must state that unbatched responses report batch_size 1 (and position 1)", file)
	}
}

// Spec: warmer metrics are graft_derived_warm_queue_depth, _dropped_total,
// _computed_total, _errors_total; eager and disabled switches exist.
func TestSpecDerivedDocWarmerMetricsAndSwitches(t *testing.T) {
	const file = "DERIVED.md"
	text := specDoc(t, file)

	specRequireContains(t, file, text,
		"graft_derived_warm_queue_depth",
		"graft_derived_warm_dropped_total",
		"graft_derived_warm_computed_total",
		"graft_derived_warm_errors_total",
		"eager",
		"disabled",
	)
}

// Spec: LIMITATIONS.md is updated for phase 3 (it must mention the batching
// and derived-data limitations rather than being left as is).
func TestSpecLimitationsMentionPhase3Features(t *testing.T) {
	const file = "LIMITATIONS.md"
	text := specDoc(t, file)

	if !specLine(text, `batch`) {
		t.Errorf("%s must mention the land batching limitations (phase 3)", file)
	}
	if !specLine(text, `derived`) {
		t.Errorf("%s must mention the derived-data limitations (phase 3)", file)
	}
}

// Spec: docs must match the code; no placeholder markers left behind from
// the drafts (TODO(graft-complete)).
func TestSpecDocsHaveNoPlaceholders(t *testing.T) {
	files := []string{"CONFIGURATION.md", "LAND-BATCHING.md", "DERIVED.md", "LIMITATIONS.md"}
	for _, f := range files {
		text := specDoc(t, f)
		if strings.Contains(text, "TODO(graft-complete)") {
			t.Errorf("%s still contains a TODO(graft-complete) placeholder", f)
		}
	}
}
