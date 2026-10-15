package changedpaths

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
)

// maxRememberedTrees bounds the trees that an ObjectReader keeps. A walk compares
// each commit with its parent, so the trees of the last few commits are the ones that
// are needed again.
const maxRememberedTrees = 256

// ObjectReader is the Objects of a repository, read through an object reader. It
// remembers the last trees that it read, which are immutable, so that a walk reads
// the tree of each commit once. It is safe for concurrent use.
type ObjectReader struct {
	Reader catfile.ObjectContentReader

	mu    sync.Mutex
	trees map[string][]Entry
}

var _ Objects = (*ObjectReader)(nil)

// NewObjectReader returns the Objects of the repository that reader reads.
func NewObjectReader(reader catfile.ObjectContentReader) *ObjectReader {
	return &ObjectReader{Reader: reader}
}

// Commit reads the commit with the given ID.
func (r *ObjectReader) Commit(ctx context.Context, id string) (Commit, error) {
	commit, err := catfile.GetCommit(ctx, r.Reader, git.Revision(id))
	if err != nil {
		return Commit{}, err
	}

	return Commit{ID: id, Tree: commit.GetTreeId(), Parents: commit.GetParentIds()}, nil
}

// Tree reads the tree with the given ID. The empty ID stands for an absent tree.
func (r *ObjectReader) Tree(ctx context.Context, id string) ([]Entry, error) {
	if id == "" {
		return nil, nil
	}

	r.mu.Lock()
	entries, ok := r.trees[id]
	r.mu.Unlock()
	if ok {
		return entries, nil
	}

	entries, err := r.readTree(ctx, id)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.trees == nil || len(r.trees) >= maxRememberedTrees {
		r.trees = make(map[string][]Entry, maxRememberedTrees)
	}
	r.trees[id] = entries
	r.mu.Unlock()

	return entries, nil
}

// readTree reads and parses the tree with the given ID.
func (r *ObjectReader) readTree(ctx context.Context, id string) ([]Entry, error) {
	object, err := r.Reader.Object(ctx, git.Revision(id))
	if err != nil {
		return nil, fmt.Errorf("reading tree %s: %w", id, err)
	}

	data, err := io.ReadAll(object)
	if err != nil {
		return nil, fmt.Errorf("reading tree %s: %w", id, err)
	}
	if object.Type != "tree" {
		return nil, fmt.Errorf("object %s is a %s, not a tree", id, object.Type)
	}

	objectHash, err := git.ObjectHashByFormat(object.Format)
	if err != nil {
		return nil, fmt.Errorf("looking up object hash: %w", err)
	}

	return parseTree(data, objectHash.Hash().Size())
}

// parseTree parses the raw content of a tree, a sequence of "<mode> <name>\x00<raw
// object ID>" entries whose object IDs have hashSize bytes.
func parseTree(data []byte, hashSize int) ([]Entry, error) {
	var entries []Entry
	for len(data) > 0 {
		space := bytes.IndexByte(data, ' ')
		if space < 0 {
			return nil, errors.New("malformed tree: missing mode")
		}
		mode, err := strconv.ParseUint(string(data[:space]), 8, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed tree: invalid mode: %w", err)
		}
		data = data[space+1:]

		nul := bytes.IndexByte(data, 0)
		if nul < 0 {
			return nil, errors.New("malformed tree: missing name")
		}
		name := string(data[:nul])
		data = data[nul+1:]

		if len(data) < hashSize {
			return nil, errors.New("malformed tree: truncated object ID")
		}
		entries = append(entries, Entry{Name: name, Mode: canonMode(mode), OID: hex.EncodeToString(data[:hashSize])})
		data = data[hashSize:]
	}

	return entries, nil
}

// canonMode normalizes a tree entry mode the way Git does when it reads trees
// (canon_mode), as the Deriver does, so that an old 100664 file equals a 100644 one.
func canonMode(mode uint64) uint32 {
	switch mode & 0o170000 {
	case 0o100000:
		if mode&0o100 != 0 {
			return 0o100755
		}
		return 0o100644
	case 0o120000:
		return 0o120000
	case 0o040000:
		return treeMode
	default:
		return 0o160000
	}
}
