package kernel

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func getRelease() (string, error) {
	uname := &unix.Utsname{}
	if err := unix.Uname(uname); err != nil {
		return "", fmt.Errorf("uname: %w", err)
	}

	return unix.ByteSliceToString(uname.Release[:]), nil
}
