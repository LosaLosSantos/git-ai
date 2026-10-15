// Package permission defines constants for Posix permission bits.
//
// Use one of the existing modes from the `mode` package unless a
// new mode truly needs to be defined.
package permission

import "io/fs"

const (
	// OwnerRead permission bit grants read permission to the owner.
	OwnerRead fs.FileMode = 0o400
	// OwnerWrite permission bit grants write permission to the owner.
	OwnerWrite fs.FileMode = 0o200
	// OwnerExecute permission bit grants execute permission to the owner.
	OwnerExecute fs.FileMode = 0o100
	// GroupRead permission bit grants read permission to the group.
	GroupRead fs.FileMode = 0o040
	// GroupWrite permission bit grants write permission to the group.
	GroupWrite fs.FileMode = 0o020
	// GroupExecute permission bit grants execute permission to the group.
	GroupExecute fs.FileMode = 0o010
	// OthersRead permission bit grants read permission to others.
	OthersRead fs.FileMode = 0o004
	// OthersWrite permission bit grants write permission to others.
	OthersWrite fs.FileMode = 0o002
	// OthersExecute permission bit grants execute permission to others.
	OthersExecute fs.FileMode = 0o001
)
