package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestTesthelperRun(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get wd: %s", err)
	}

	testdata := filepath.Join(wd, "testdata")
	analyzer := newTesthelperRunAnalyzer(&testhelperRunAnalyzerSettings{IncludedFunctions: []string{
		"testhelper_run_not_testhelper.Run",
	}})
	analysistest.Run(
		t,
		testdata,
		analyzer,
		"testhelper_run_no_tests",
		"testhelper_run_no_testmain",
		"testhelper_run_no_exec_testmain",
		"testhelper_run_not_testhelper",
		"testhelper_run_blackbox",
	)
}
