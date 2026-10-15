package permission_test

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/storage/mode/permission"
	"github.com/stretchr/testify/require"
)

func TestOwnerRead(t *testing.T) {
	require.Equal(t, "-r--------", permission.OwnerRead.String())
}

func TestOwnerWrite(t *testing.T) {
	require.Equal(t, "--w-------", permission.OwnerWrite.String())
}

func TestOwnerExecute(t *testing.T) {
	require.Equal(t, "---x------", permission.OwnerExecute.String())
}

func TestGroupRead(t *testing.T) {
	require.Equal(t, "----r-----", permission.GroupRead.String())
}

func TestGroupWrite(t *testing.T) {
	require.Equal(t, "-----w----", permission.GroupWrite.String())
}

func TestGroupExecute(t *testing.T) {
	require.Equal(t, "------x---", permission.GroupExecute.String())
}

func TestOthersRead(t *testing.T) {
	require.Equal(t, "-------r--", permission.OthersRead.String())
}

func TestOthersWrite(t *testing.T) {
	require.Equal(t, "--------w-", permission.OthersWrite.String())
}

func TestOthersExecute(t *testing.T) {
	require.Equal(t, "---------x", permission.OthersExecute.String())
}
