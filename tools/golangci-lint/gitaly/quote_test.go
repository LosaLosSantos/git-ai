package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestQuoteInterpolationAnalyzer(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get wd: %s", err)
	}

	testdata := filepath.Join(wd, "testdata")
	analyzer := newQuoteInterpolationAnalyzer(&quoteInterpolationAnalyzerSettings{IncludedFunctions: []string{
		"fmt.*",
	}})
	analysistest.Run(t, testdata, analyzer, "quote")
}
