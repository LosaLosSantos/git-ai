package testhelper_run_not_testhelper // want package:"package has TestMain"

import "testing"

func Run(m *testing.M) {}

func TestMain(m *testing.M) { // want "TestMain should be placed in file 'testhelper_test.go'"
	Run(m)
}
