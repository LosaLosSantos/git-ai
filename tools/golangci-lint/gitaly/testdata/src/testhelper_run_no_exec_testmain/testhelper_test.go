package testhelper_run_no_exec_testmain // want package:"package has TestMain"

import (
	"testing"
)

func TestMain(m *testing.M) { // want "testhelper.Run not called in TestMain"
}
