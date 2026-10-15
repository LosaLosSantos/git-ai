package testhelper_run_blackbox_test // want "no TestMain in package testhelper_run_blackbox_test"

import (
	"testhelper_run_blackbox"
	"testing"
)

func TestFoo(t *testing.T) {
	if testhelper_run_blackbox.Foo() != "bar" {
		t.FailNow()
	}
}
