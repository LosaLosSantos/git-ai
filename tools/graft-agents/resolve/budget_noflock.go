//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package resolve

import "errors"

var errNoFlock = errors.New("budget: a state file needs flock, which this platform lacks")

func checkFlock() error { return errNoFlock }

func lockFile(string) (func(), error) { return nil, errNoFlock }
