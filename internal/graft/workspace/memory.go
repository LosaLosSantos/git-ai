package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// zeroOID is the object ID used for a reference that did not exist.
var zeroOID = strings.Repeat("0", 40)

type memFile struct {
	content []byte
	mode    uint32
}

type memCommit struct {
	id      string
	parent  string
	author  string
	subject string
	files   map[string]memFile
}

// MemoryBackend is an in-memory Backend for tests and offline experiments. It
// keeps a linear history per branch and always lands by rebasing, so Rewritten
// is true for diverged lands. It keeps no per-line history, so Blame attributes
// every line to the revision.
type MemoryBackend struct {
	mu         sync.Mutex
	commits    map[string]*memCommit
	branches   map[string]string
	objects    map[string][]byte
	changesets []Changeset
	next       int
}

// NewMemoryBackend returns an empty in-memory repository.
func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{
		commits:  map[string]*memCommit{},
		branches: map[string]string{},
		objects:  map[string][]byte{},
	}
}

// Put commits a regular file on branch, creating the branch when needed. It
// stands in for changes made by other agents and returns the new commit ID.
func (m *MemoryBackend) Put(branch, path string, content []byte) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	parent := m.branches[branch]
	files := map[string]memFile{}
	if parent != "" {
		files = cloneFiles(m.commits[parent].files)
	}
	files[path] = memFile{content: append([]byte(nil), content...), mode: modeRegular}
	id := m.newCommit(parent, files, "memory", "put "+path)
	m.setBranch(branch, id, "receive_pack", false)
	return id
}

// ResolveRevision implements Backend.
func (m *MemoryBackend) ResolveRevision(_ context.Context, rev string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolve(rev)
}

// ListTree implements Backend.
func (m *MemoryBackend) ListTree(_ context.Context, rev, path string, recursive bool) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, err := m.resolve(rev)
	if err != nil {
		return nil, err
	}
	files := m.commits[id].files
	if _, isFile := files[path]; isFile && path != "" {
		// Like the server, a file is not a tree. This is not ErrNotFound, so
		// callers can tell a file prefix from a missing one.
		return nil, fmt.Errorf("path %q at %s is a file, not a tree", path, rev)
	}
	prefix := ""
	if path != "" {
		prefix = path + "/"
	}
	found := path == ""
	seenDirs := map[string]bool{}
	var out []Entry
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		found = true
		rel := strings.TrimPrefix(name, prefix)
		if !recursive && strings.Contains(rel, "/") {
			dir := rel[:strings.Index(rel, "/")]
			if !seenDirs[dir] {
				seenDirs[dir] = true
				out = append(out, Entry{Path: dir, Type: ObjectTree, Mode: modeTree})
			}
			continue
		}
		f := files[name]
		out = append(out, Entry{
			Path: rel,
			OID:  BlobID(f.content, false),
			Type: ObjectBlob,
			Mode: f.mode,
			Size: int64(len(f.content)),
		})
	}
	if !found {
		return nil, fmt.Errorf("path %q at %s: %w", path, rev, ErrNotFound)
	}
	return out, nil
}

// GetObjects implements Backend.
func (m *MemoryBackend) GetObjects(_ context.Context, oids []string) (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string][]byte, len(oids))
	for _, oid := range oids {
		if content, ok := m.objects[oid]; ok {
			out[oid] = append([]byte(nil), content...)
		}
	}
	return out, nil
}

// ReadFile implements Backend.
func (m *MemoryBackend) ReadFile(_ context.Context, rev, path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, err := m.resolve(rev)
	if err != nil {
		return nil, err
	}
	files := m.commits[id].files
	f, ok := files[path]
	if !ok {
		if hasFileUnder(files, path) {
			// Like the server, a directory is not a file. This is not ErrNotFound.
			return nil, fmt.Errorf("path %q at %s is a directory, not a file", path, rev)
		}
		return nil, fmt.Errorf("path %q at %s: %w", path, rev, ErrNotFound)
	}
	return append([]byte(nil), f.content...), nil
}

// hasFileUnder reports whether a file lies under the directory dir.
func hasFileUnder(files map[string]memFile, dir string) bool {
	for name := range files {
		if strings.HasPrefix(name, dir+"/") {
			return true
		}
	}
	return false
}

// fileAncestor returns an ancestor directory of path that is a file, if any.
func fileAncestor(files map[string]memFile, path string) (string, bool) {
	for i := 0; i < len(path); i++ {
		if path[i] != '/' {
			continue
		}
		if _, ok := files[path[:i]]; ok {
			return path[:i], true
		}
	}
	return "", false
}

// CommitFiles implements Backend.
func (m *MemoryBackend) CommitFiles(_ context.Context, opts CommitOptions) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if opts.Branch == "" {
		return "", errors.New("branch is required")
	}
	startID, err := m.resolve(opts.StartRev)
	if err != nil {
		return "", fmt.Errorf("start revision: %w", err)
	}
	files := cloneFiles(m.commits[startID].files)
	for _, a := range opts.Actions {
		switch a.Kind {
		case ActionPut:
			existing, exists := files[a.Path]
			if exists && !a.Overwrite {
				return "", fmt.Errorf("put %q: %w", a.Path, ErrAlreadyExists)
			}
			// Like the server, actions apply in order: a file cannot replace a
			// directory that still holds files, nor lie under a file.
			if hasFileUnder(files, a.Path) {
				return "", fmt.Errorf("put %q: a directory with this name exists", a.Path)
			}
			if ancestor, ok := fileAncestor(files, a.Path); ok {
				return "", fmt.Errorf("put %q: %q is a file", a.Path, ancestor)
			}
			// As on the server, Executable only applies to a new file: an
			// overwrite keeps the mode of the file it replaces. A mode change is a
			// Chmod action.
			mode := modeRegular
			if a.Executable {
				mode = modeExec
			}
			if exists {
				mode = existing.mode
			}
			files[a.Path] = memFile{content: append([]byte(nil), a.Content...), mode: mode}
		case ActionDelete:
			if _, ok := files[a.Path]; !ok {
				return "", fmt.Errorf("delete %q: %w", a.Path, ErrNotFound)
			}
			delete(files, a.Path)
		case ActionMove:
			f, ok := files[a.FromPath]
			if !ok {
				return "", fmt.Errorf("move %q: %w", a.FromPath, ErrNotFound)
			}
			delete(files, a.FromPath)
			files[a.Path] = f
		case ActionChmod:
			f, ok := files[a.Path]
			if !ok {
				return "", fmt.Errorf("chmod %q: %w", a.Path, ErrNotFound)
			}
			if a.Executable {
				f.mode = modeExec
			} else {
				f.mode = modeRegular
			}
			files[a.Path] = f
		default:
			return "", fmt.Errorf("unknown action %q", a.Kind)
		}
	}
	id := m.newCommit(startID, files, opts.Author.Name, firstLine(opts.Message))
	m.setBranch(opts.Branch, id, "commit_files", false)
	return id, nil
}

// Land implements Backend.
func (m *MemoryBackend) Land(_ context.Context, opts LandOptions) (LandResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, err := m.resolve(opts.Commit)
	if err != nil {
		return LandResult{}, err
	}
	tip, exists := m.branches[opts.Branch]
	if !exists {
		if !opts.CreateBranch {
			return LandResult{}, fmt.Errorf("branch %q: %w", opts.Branch, ErrNotFound)
		}
		m.setBranch(opts.Branch, id, "land", false)
		return LandResult{Commit: id}, nil
	}
	if tip == id || m.isAncestor(id, tip) {
		return LandResult{Commit: tip, AlreadyLanded: true}, nil
	}
	if m.isAncestor(tip, id) {
		m.setBranch(opts.Branch, id, "land", false)
		return LandResult{Commit: id}, nil
	}
	if opts.Strategy == StrategyFFOnly {
		return LandResult{}, ErrNotFastForward
	}

	commit := m.commits[id]
	base := map[string]memFile{}
	if commit.parent != "" {
		base = m.commits[commit.parent].files
	}
	// Three-way merge of the commit's changes (relative to its parent) onto the tip. With a
	// resolution, the conflicting names take the resolved files, as the server's merge commit
	// does; the resolution must be made against this tip and resolve exactly those names.
	if opts.Resolution != nil && opts.Resolution.Tip != tip {
		return LandResult{}, fmt.Errorf("the resolution was made against tip %s, the branch is at %s: %w", opts.Resolution.Tip, tip, ErrMergeConflict)
	}
	resolved := map[string]ResolvedFile{}
	if opts.Resolution != nil {
		for _, f := range opts.Resolution.Files {
			resolved[f.Path] = f
		}
	}
	conflicting := map[string]bool{}
	files := cloneFiles(m.commits[tip].files)
	names := map[string]bool{}
	for name := range commit.files {
		names[name] = true
	}
	for name := range base {
		names[name] = true
	}
	for name := range names {
		commitFile, commitHasFile := commit.files[name]
		baseFile, baseHas := base[name]
		if sameFile(commitFile, commitHasFile, baseFile, baseHas) {
			continue
		}
		tipFile, tipHas := files[name]
		if !sameFile(tipFile, tipHas, baseFile, baseHas) && !sameFile(tipFile, tipHas, commitFile, commitHasFile) {
			if opts.Resolution == nil {
				return LandResult{}, fmt.Errorf("%s: %w", name, ErrMergeConflict)
			}
			r, ok := resolved[name]
			if !ok {
				return LandResult{}, fmt.Errorf("%s is not resolved: %w", name, ErrMergeConflict)
			}
			conflicting[name] = true
			if r.Delete {
				delete(files, name)
				continue
			}
			mode, err := strconv.ParseUint(r.Mode, 8, 32)
			if err != nil {
				return LandResult{}, fmt.Errorf("the resolution of %s has the mode %q: %w", name, r.Mode, err)
			}
			files[name] = memFile{content: r.Content, mode: uint32(mode)}
			continue
		}
		if commitHasFile {
			files[name] = commitFile
		} else {
			delete(files, name)
		}
	}
	for name := range resolved {
		if !conflicting[name] {
			return LandResult{}, fmt.Errorf("the resolution names %s, which does not conflict: %w", name, ErrMergeConflict)
		}
	}
	newID := m.newCommit(tip, files, commit.author, commit.subject)
	m.setBranch(opts.Branch, newID, "land", true)
	return LandResult{Commit: newID, Rewritten: true}, nil
}

// DeleteBranch implements Backend.
func (m *MemoryBackend) DeleteBranch(_ context.Context, branch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.branches, branch)
	return nil
}

// Changesets implements Backend.
func (m *MemoryBackend) Changesets(_ context.Context, after uint64, limit int) ([]Changeset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changesetsAfter(after, limit), nil
}

// WatchChangesets implements Backend. The memory backend has no live updates:
// the channel carries the backlog and then closes.
func (m *MemoryBackend) WatchChangesets(_ context.Context, after uint64) (<-chan Changeset, <-chan error) {
	m.mu.Lock()
	backlog := m.changesetsAfter(after, 0)
	m.mu.Unlock()

	changes := make(chan Changeset, len(backlog))
	for _, c := range backlog {
		changes <- c
	}
	close(changes)
	errs := make(chan error)
	close(errs)
	return changes, errs
}

// Blame implements Backend.
func (m *MemoryBackend) Blame(_ context.Context, rev, path string) ([]BlameLine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, err := m.resolve(rev)
	if err != nil {
		return nil, err
	}
	f, ok := m.commits[id].files[path]
	if !ok {
		return nil, fmt.Errorf("path %q at %s: %w", path, rev, ErrNotFound)
	}
	var out []BlameLine
	for i, line := range splitLines(f.content) {
		out = append(out, BlameLine{
			LineNumber:         i + 1,
			CommitID:           id,
			OriginalLineNumber: i + 1,
			Content:            []byte(line),
		})
	}
	return out, nil
}

// PathHistory implements Backend.
func (m *MemoryBackend) PathHistory(_ context.Context, rev, path string, limit int) ([]Commit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, err := m.resolve(rev)
	if err != nil {
		return nil, err
	}
	var out []Commit
	for id != "" && (limit <= 0 || len(out) < limit) {
		c := m.commits[id]
		f, has := c.files[path]
		var old memFile
		oldHas := false
		if c.parent != "" {
			old, oldHas = m.commits[c.parent].files[path]
		}
		if has != oldHas || (has && (!bytes.Equal(f.content, old.content) || f.mode != old.mode)) {
			out = append(out, Commit{ID: c.id, AuthorName: c.author, Subject: c.subject})
		}
		id = c.parent
	}
	return out, nil
}

func (m *MemoryBackend) resolve(rev string) (string, error) {
	if id, ok := m.branches[rev]; ok {
		return id, nil
	}
	if _, ok := m.commits[rev]; ok {
		return rev, nil
	}
	return "", fmt.Errorf("revision %q: %w", rev, ErrNotFound)
}

func (m *MemoryBackend) newCommit(parent string, files map[string]memFile, author, subject string) string {
	m.next++
	id := fmt.Sprintf("%040x", m.next)
	m.commits[id] = &memCommit{id: id, parent: parent, author: author, subject: subject, files: files}
	for _, f := range files {
		m.objects[BlobID(f.content, false)] = f.content
	}
	return id
}

func (m *MemoryBackend) setBranch(branch, id, operation string, rewritten bool) {
	old := zeroOID
	if prev, ok := m.branches[branch]; ok {
		old = prev
	}
	m.branches[branch] = id
	m.changesets = append(m.changesets, Changeset{
		Sequence:   uint64(len(m.changesets) + 1),
		Reference:  "refs/heads/" + branch,
		OldOID:     old,
		NewOID:     id,
		Operation:  operation,
		Actor:      "memory",
		RecordedAt: time.Now().UTC(),
		Rewritten:  rewritten,
	})
}

func (m *MemoryBackend) changesetsAfter(after uint64, limit int) []Changeset {
	var out []Changeset
	for _, c := range m.changesets {
		if c.Sequence > after {
			out = append(out, c)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out
}

func (m *MemoryBackend) isAncestor(ancestor, descendant string) bool {
	for id := descendant; id != ""; id = m.commits[id].parent {
		if id == ancestor {
			return true
		}
	}
	return false
}

// sameFile reports whether two optional files are equal, including absence.
func sameFile(a memFile, aHas bool, b memFile, bHas bool) bool {
	if aHas != bHas {
		return false
	}
	return !aHas || (bytes.Equal(a.content, b.content) && a.mode == b.mode)
}

func cloneFiles(files map[string]memFile) map[string]memFile {
	out := make(map[string]memFile, len(files))
	for name, f := range files {
		out[name] = f
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
