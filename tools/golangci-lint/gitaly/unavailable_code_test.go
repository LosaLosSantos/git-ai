package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestUnavailableCode(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get wd: %s", err)
	}

	testdata := filepath.Join(wd, "testdata")
	analyzer := newUnavailableCodeAnalyzer(&unavailableCodeAnalyzerSettings{IncludedFunctions: []string{
		"unavailable_code.NewUnavailable",
	}})
	analysistest.Run(t, testdata, analyzer, "unavailable_code")
}
