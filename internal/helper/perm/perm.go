package perm

import (
	"io/fs"
)

// Umask represents a umask that is used to mask mode bits.
type Umask int

// Mask applies the mask on the mode.
func (mask Umask) Mask(mode fs.FileMode) fs.FileMode {
	return mode & ^fs.FileMode(mask)
}
