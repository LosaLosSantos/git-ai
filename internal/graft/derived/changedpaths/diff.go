// Package changedpaths computes, for each commit, the files that differ from its
// first parent, and answers first-parent path histories from those sets without
// comparing trees for every commit. It is the changed-paths kind of the derived
// data framework (see doc/graft/PLAN3.md, WP27).
//
// The index itself is independent of Git's object access: Objects abstracts the
// commit and tree reads, and Store abstracts persistence. ObjectReader and NewIndexFor
// adapt them to catfile and to the derived-data store, and Warm is the eager warmer
// of the kind (registered in init).
package changedpaths

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// treeMode is the canonical mode of a tree entry.
const treeMode uint32 = 0o040000

// Entry is an entry of a tree. Mode is canonical, as Git normalizes modes when it
// reads trees: treeMode for trees, and otherwise 0o100644, 0o100755, 0o120000 or
// 0o160000 for blobs, symlinks and gitlinks.
type Entry struct {
	Name string
	Mode uint32
	OID  string
}

func (e Entry) isTree() bool { return e.Mode == treeMode }

// Commit holds what the index reads of a commit.
type Commit struct {
	ID      string
	Tree    string
	Parents []string
}

// Objects reads commits and trees by object ID. The empty tree ID "" stands for an
// absent tree and has no entries.
type Objects interface {
	Commit(ctx context.Context, id string) (Commit, error)
	Tree(ctx context.Context, id string) ([]Entry, error)
}

// dirPair is a pending comparison of two directories. Prefix is the path of the
// directories relative to the root and ends with a slash, or is empty for the root.
// An empty tree ID means that the directory is absent on that side.
type dirPair struct {
	prefix string
	a, b   string
}

// changedFiles returns the sorted paths of the non-tree entries that differ between
// the trees oldTree and newTree. An entry differs when it is present on only one
// side or when its object ID or canonical mode differs. That is the TREESAME
// relation of git log -- path for files, and directories are never reported, only
// the files they hold.
//
// When filter is not empty, only the paths equal to filter or below it are reported.
// When limit is positive and more than limit paths differ, changedFiles stops early
// and reports overflow with no paths.
func changedFiles(ctx context.Context, objects Objects, oldTree, newTree, filter string, limit int) ([]string, bool, error) {
	var paths []string
	stack := []dirPair{{a: oldTree, b: newTree}}

	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}

		pair := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if pair.a == pair.b {
			continue
		}

		entriesA, err := treeEntries(ctx, objects, pair.a)
		if err != nil {
			return nil, false, err
		}
		entriesB, err := treeEntries(ctx, objects, pair.b)
		if err != nil {
			return nil, false, err
		}

		for _, name := range allNames(entriesA, entriesB) {
			full := pair.prefix + name
			if filter != "" && !related(full, filter) {
				continue
			}

			a, inA := entriesA[name]
			b, inB := entriesB[name]
			if inA && inB && a == b {
				continue
			}

			aTree := inA && a.isTree()
			bTree := inB && b.isTree()
			if aTree && bTree {
				if a.OID != b.OID {
					stack = append(stack, dirPair{prefix: full + "/", a: a.OID, b: b.OID})
				}
				continue
			}

			if ((inA && !aTree) || (inB && !bTree)) && covered(full, filter) {
				if limit > 0 && len(paths) == limit {
					return nil, true, nil
				}
				paths = append(paths, full)
			}
			// A tree on one side only (or a tree replaced by a file) contributes all the
			// files it holds, which compare against an absent directory.
			if aTree {
				stack = append(stack, dirPair{prefix: full + "/", a: a.OID})
			}
			if bTree {
				stack = append(stack, dirPair{prefix: full + "/", b: b.OID})
			}
		}
	}

	sort.Strings(paths)
	return paths, false, nil
}

// treeEntries returns the entries of the tree id by name. The empty ID has none.
func treeEntries(ctx context.Context, objects Objects, id string) (map[string]Entry, error) {
	if id == "" {
		return nil, nil
	}

	entries, err := objects.Tree(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading tree %s: %w", id, err)
	}

	byName := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		byName[entry.Name] = entry
	}
	return byName, nil
}

// allNames returns the names present in either map, in no particular order.
func allNames(a, b map[string]Entry) []string {
	names := make([]string, 0, len(a)+len(b))
	for name := range a {
		names = append(names, name)
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			names = append(names, name)
		}
	}
	return names
}

// related reports whether a directory or file at path can hold or be the filter:
// path equals filter, lies below it, or is an ancestor of it. Only such entries
// need to be visited.
func related(path, filter string) bool {
	return path == filter || strings.HasPrefix(path, filter+"/") || strings.HasPrefix(filter, path+"/")
}

// covered reports whether path is reported for filter: the empty filter covers
// everything, and otherwise path must equal filter or lie below it. An ancestor of
// the filter is related but not covered, since it is a directory or a file that
// does not contain the filter.
func covered(path, filter string) bool {
	return filter == "" || path == filter || strings.HasPrefix(path, filter+"/")
}
