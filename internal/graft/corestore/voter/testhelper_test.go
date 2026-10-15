package voter_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/testhelper"
)

// TestMain runs the tests of the package with reftable as the default
// reference backend: authoritative storage mode serves reftable repositories
// only (doc/graft/PLAN6.md, REFTABLE RULE). testhelper.Run turns
// GITALY_TEST_REF_FORMAT into GIT_DEFAULT_REF_FORMAT, which every git process
// of the tests and of the test servers honours. The tests of the files backend
// create their repositories with an explicit --ref-format=files.
func TestMain(m *testing.M) {
	if err := os.Setenv("GITALY_TEST_REF_FORMAT", git.ReferenceBackendReftables.Name); err != nil {
		fmt.Printf("setting the reference backend of the tests: %v\n", err)
		os.Exit(1)
	}
	gittest.DefaultReferenceBackend = git.ReferenceBackendReftables

	testhelper.Run(m)
}
