package corestore

import (
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/protobuf/proto"
)

// ReviewNeedsImport reports whether MetaStore.ImportHistory writes the file
// review file into the database, given the database review db with the same
// ID (nil when the database has none). It is the single rule that
// ImportHistory applies and that the authoritative-mode gate checks before
// serving a repository (doc/graft/PLAN6.md, LOCK AND HISTORY RULE B):
//
//   - The database has no review with that ID: the review was created in
//     shadow mode after the first import.
//   - The database review is the same review (same creation time: it was
//     copied from the file by an earlier import) and the file was changed
//     after it (a later update time): it was changed in shadow mode, where the
//     file review store is the source of truth and the database is never
//     written.
//
// A database review that changed after the file (in authoritative mode), an
// unchanged one, and a different review that only shares the ID are kept.
func ReviewNeedsImport(file, db *gitalypb.Review) bool {
	if db == nil {
		return true
	}

	if !proto.Equal(file.GetCreatedAt(), db.GetCreatedAt()) {
		return false
	}

	if file.GetUpdatedAt() == nil {
		return false
	}
	if db.GetUpdatedAt() == nil {
		return true
	}

	return file.GetUpdatedAt().AsTime().After(db.GetUpdatedAt().AsTime())
}
