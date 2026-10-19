//go:build !cgo

package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestBenchRefusesWithoutCgo: without the grammars, mining could not validate the human
// resolutions and the syntax check would reject every answer, so bench mine, run and syntax refuse
// to start, naming the build requirement.
func TestBenchRefusesWithoutCgo(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"bench", "mine", "--corpus", dir + "/corpus.json", "--clones", dir, "--manifest", dir + "/m.json"},
		{"bench", "run", "--manifest", dir + "/m.json", "--clones", dir},
		{"bench", "syntax", "--manifest", dir + "/m.json", "--clones", dir},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, nil, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "CGO_ENABLED=1") {
			t.Errorf("%s exited %d: %s", args[1], code, stderr.String())
		}
	}
}
