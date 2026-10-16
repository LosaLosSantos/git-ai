package derived

import (
	"strconv"

	"github.com/feder-cr/git-ai/internal/git"
)

// KindStore is a typed view of a Store for one kind and one version of that kind.
// Values are keyed by commit alone, so a value is a pure function of its commit.
// Bump the version when the encoding or the computation changes: entries of other
// versions are then never read.
//
// The zero KindStore is not usable. Obtain one with Store.Kind. A nil Store yields a
// KindStore that never finds a value and discards writes.
type KindStore struct {
	store   *Store
	name    string
	version int
}

// Kind returns the view of kind name at version. The name must match the store's
// kind syntax (lowercase letters and hyphens); otherwise Get and Put return an
// error.
func (s *Store) Kind(name string, version int) KindStore {
	return KindStore{store: s, name: name, version: version}
}

// key returns the store key of the value of commit. The version is part of the
// hashed key, so entries of different versions never collide.
func (k KindStore) key(commit git.ObjectID) string {
	return Key(k.name+"@v"+strconv.Itoa(k.version), commit, "")
}

// Get returns the value stored for commit. The boolean is false when no value is
// stored.
func (k KindStore) Get(commit git.ObjectID) ([]byte, bool, error) {
	return k.store.Get(k.name, k.key(commit))
}

// Put stores the value of commit, replacing any entry already stored.
func (k KindStore) Put(commit git.ObjectID, value []byte) error {
	return k.store.Put(k.name, k.key(commit), value)
}
