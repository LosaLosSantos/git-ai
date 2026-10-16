// mode_test is an external test package to avoid cyclic dependencies between the
// `testhelper` package and the `mode` packages tests. `testhelper` imports the
// `mode` constants. `mode` package tests need to import `testhelper` as every
// package's tests must call `testhelper.Run` in `TestMain`.
package mode_test

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/stretchr/testify/require"
)

func TestDirectory(t *testing.T) {
	require.Equal(t, "drwx------", mode.Directory.String())
}

func TestExecutable(t *testing.T) {
	require.Equal(t, "-r-x------", mode.Executable.String())
}

func TestFile(t *testing.T) {
	require.Equal(t, "-r--------", mode.File.String())
}
