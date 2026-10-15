package changedpaths

import (
	"context"
	"fmt"
)

// fakeObjects is an in-memory Objects. Tree IDs are assigned by tree.
type fakeObjects struct {
	commits   map[string]Commit
	trees     map[string][]Entry
	treeReads int
}

func newFakeObjects() *fakeObjects {
	return &fakeObjects{commits: map[string]Commit{}, trees: map[string][]Entry{}}
}

func (f *fakeObjects) Commit(_ context.Context, id string) (Commit, error) {
	commit, ok := f.commits[id]
	if !ok {
		return Commit{}, fmt.Errorf("no commit %q", id)
	}
	return commit, nil
}

func (f *fakeObjects) Tree(_ context.Context, id string) ([]Entry, error) {
	f.treeReads++
	entries, ok := f.trees[id]
	if !ok {
		return nil, fmt.Errorf("no tree %q", id)
	}
	return entries, nil
}

// tree stores the entries as a new tree and returns its ID.
func (f *fakeObjects) tree(entries ...Entry) string {
	id := fmt.Sprintf("tree%d", len(f.trees))
	f.trees[id] = entries
	return id
}

// commit stores a commit with the given tree and parents and returns its ID.
func (f *fakeObjects) commit(tree string, parents ...string) string {
	id := fmt.Sprintf("commit%d", len(f.commits))
	f.commits[id] = Commit{ID: id, Tree: tree, Parents: parents}
	return id
}

func blob(name, oid string) Entry    { return Entry{Name: name, Mode: 0o100644, OID: oid} }
func subtree(name, oid string) Entry { return Entry{Name: name, Mode: treeMode, OID: oid} }

// memStore is an in-memory Store.
type memStore struct {
	data map[string][]byte
}

func newMemStore() *memStore { return &memStore{data: map[string][]byte{}} }

func (m *memStore) Get(key string) ([]byte, bool, error) {
	data, ok := m.data[key]
	return data, ok, nil
}

func (m *memStore) Put(key string, data []byte) error {
	m.data[key] = append([]byte(nil), data...)
	return nil
}
