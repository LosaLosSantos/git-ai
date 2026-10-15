package gittest

import (
	"os"
	"testing"

	"github.com/feder-cr/open_scion/internal/git"
)

// DefaultReferenceBackend is the default reftable backend used for running tests.
var DefaultReferenceBackend = func() git.ReferenceBackend {
	if val, enabled := os.LookupEnv("GITALY_TEST_REF_FORMAT"); enabled && val == "reftable" {
		return git.ReferenceBackendReftables
	}

	return git.ReferenceBackendFiles
}()

// FilesOrReftables returns the files or reftable value based on which reference
// backend is currently being used.
func FilesOrReftables[T any](files, reftable T) T {
	if DefaultReferenceBackend == git.ReferenceBackendFiles {
		return files
	}
	return reftable
}

// SkipWithoutMVCCGit skips the test when the bundled MVCC Git binary is not
// available. This happens when GITALY_TESTING_GIT_BINARY overrides the Git
// binary, for example in the nightly tests.
func SkipWithoutMVCCGit(tb testing.TB) {
	if os.Getenv("GITALY_TESTING_GIT_BINARY") != "" {
		tb.Skip("the MVCC reference backend requires the bundled MVCC Git binary, which is disabled by the GITALY_TESTING_GIT_BINARY override")
	}
}
