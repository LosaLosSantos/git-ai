package changelog

import "context"

// Source is where the changelog of one repository is read from: the file log
// of the repository (FileSource), or the storage core's database in
// authoritative mode (internal/graft/corestore/dblog).
type Source interface {
	// Scan calls fn for every entry with a sequence greater than after, in
	// order, until fn returns false or an error (which Scan returns).
	Scan(ctx context.Context, after uint64, fn func(Entry) (bool, error)) error
	// Subscribe returns a channel that receives a coalesced value after new
	// entries may have been appended, and a function that ends the
	// subscription. Watchers also poll, so a missed wake-up only delays them.
	Subscribe() (<-chan struct{}, func())
}

// FileSource is the Source of the file log of the repository at repoPath.
func FileSource(repoPath string) Source { return fileSource{repoPath: repoPath} }

type fileSource struct{ repoPath string }

func (s fileSource) Scan(ctx context.Context, after uint64, fn func(Entry) (bool, error)) error {
	return Scan(s.repoPath, after, fn)
}

func (s fileSource) Subscribe() (<-chan struct{}, func()) { return Subscribe(s.repoPath) }
