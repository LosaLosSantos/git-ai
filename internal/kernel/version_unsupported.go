//go:build !linux

package kernel

import "errors"

func getRelease() (string, error) {
	return "", errors.New("kernel version detection not supported")
}
