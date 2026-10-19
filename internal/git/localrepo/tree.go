package localrepo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// Entries holds every ls-tree Entry
type Entries []TreeEntry

func (e Entries) Len() int {
	return len(e)
}

func (e Entries) Swap(i, j int) {
	e[i], e[j] = e[j], e[i]
}

// Less sorts entries by type in the order [*tree *blobs *submodules]
func (e Entries) Less(i, j int) bool {
	return e[i].Type < e[j].Type
}

// TreeEntriesByPath allows a slice of *TreeEntry to be sorted by Path
type TreeEntriesByPath []*TreeEntry

func (b TreeEntriesByPath) Len() int {
	return len(b)
}

func (b TreeEntriesByPath) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}

func (b TreeEntriesByPath) Less(i, j int) bool {
	iPath, jPath := b[i].Path, b[j].Path

	// git has an edge case for subtrees where they are always appended with
	// a '/'. See https://github.com/git/git/blob/v2.40.0/read-cache.c#L491
	if b[i].Type == Tree {
		iPath += "/"
	}

	if b[j].Type == Tree {
		jPath += "/"
	}

	return iPath < jPath
}

// TreeEntry represents an entry of a git tree object.
type TreeEntry struct {
	// OID is the object ID the tree entry refers to.
	OID git.ObjectID
	// Mode is the file mode of the tree entry.
	Mode string
	// Path is the full path of the tree entry.
	Path string
	// Type is the type of the tree entry.
	Type ObjectType
	// Entries is a slice of this tree's entries.
	Entries []*TreeEntry

	// loader, set on a tree that ReadTreeLazily read without its entries, reads them when an
	// operation first descends into the tree. Nil once they are read, and for every tree of
	// ReadTree.
	loader *treeLoader
}

// treeLoader reads the entries of the trees of a tree read by ReadTreeLazily through the object
// reader of the request (PLAN9, WP96): only the trees that the operations descend into are read.
type treeLoader struct {
	ctx        context.Context
	reader     catfile.ObjectContentReader
	objectHash git.ObjectHash
}

// ensureLoaded reads the entries of t if it is a tree of ReadTreeLazily whose entries were not read
// yet. It does nothing for any other entry.
func (t *TreeEntry) ensureLoaded() error {
	if t.loader == nil {
		return nil
	}

	entries, err := t.loader.read(t.OID)
	if err != nil {
		return err
	}
	t.Entries = entries
	t.loader = nil
	return nil
}

// read returns the entries of the tree oid as ls-tree describes them: a six-digit mode, the type
// derived from the mode and the base name. Its subtrees are left to be read on demand.
func (l *treeLoader) read(oid git.ObjectID) ([]*TreeEntry, error) {
	object, err := l.reader.Object(l.ctx, oid.Revision())
	if err != nil {
		return nil, fmt.Errorf("reading tree %s: %w", oid, err)
	}
	// The object must be consumed fully before the reader serves another request.
	data, err := io.ReadAll(object)
	if err != nil {
		return nil, fmt.Errorf("reading tree %s: %w", oid, err)
	}
	if object.Type != "tree" {
		return nil, fmt.Errorf("reading tree %s: %w", oid, ErrNotTreeish)
	}

	rawSize := l.objectHash.EncodedLen() / 2
	var entries []*TreeEntry
	for len(data) > 0 {
		mode, rest, ok := bytes.Cut(data, []byte{' '})
		if !ok {
			return nil, fmt.Errorf("parsing tree %s: no mode", oid)
		}
		name, rest, ok := bytes.Cut(rest, []byte{0})
		if !ok || len(rest) < rawSize {
			return nil, fmt.Errorf("parsing tree %s: truncated entry", oid)
		}
		entryOID, err := l.objectHash.FromHex(hex.EncodeToString(rest[:rawSize]))
		if err != nil {
			return nil, fmt.Errorf("parsing tree %s: %w", oid, err)
		}
		data = rest[rawSize:]

		// As ls-tree prints it: the octal value with six digits, and the type from its format
		// bits.
		modeValue, err := strconv.ParseUint(string(mode), 8, 32)
		if err != nil {
			return nil, fmt.Errorf("parsing tree %s: invalid mode %q", oid, mode)
		}
		entry := &TreeEntry{
			OID:  entryOID,
			Mode: fmt.Sprintf("%06o", modeValue),
			Path: string(name),
		}
		switch modeValue & 0o170000 {
		case 0o040000:
			entry.Type = Tree
			entry.loader = l
		case 0o160000:
			entry.Type = Submodule
		default:
			entry.Type = Blob
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

// IsBlob returns whether or not the TreeEntry is a blob.
func (t *TreeEntry) IsBlob() bool {
	return t.Type == Blob
}

var (
	// ErrTreeNotExist indicates that the requested tree does not exist, either because the revision
	// is invalid or because the path is not valid.
	ErrTreeNotExist = errors.New("invalid object name")
	// ErrNotTreeish indicates that the requested revision or path does not resolve to a tree
	// object.
	ErrNotTreeish = errors.New("not treeish")
	// ErrEntryNotFound indicates an entry could not be found.
	ErrEntryNotFound = errors.New("entry not found")
	// ErrEntryExists indicates an entry already exists.
	ErrEntryExists = errors.New("entry already exists")
	// ErrPathTraversal indicates a path contains a traversal.
	ErrPathTraversal = errors.New("path contains traversal")
	// ErrAbsolutePath indicates the path is an absolute path
	ErrAbsolutePath = errors.New("path is absolute")
	// ErrEmptyPath indicates the path is an absolute path
	ErrEmptyPath = errors.New("path is empty")
	// ErrDisallowedPath indicates the path is invalid
	ErrDisallowedPath = errors.New("disallowed path")
)

func validateFileCreationPath(path string) (string, error) {
	path = filepath.Clean(path)

	if filepath.IsAbs(path) {
		return "", ErrAbsolutePath
	}

	if strings.HasPrefix(path, "..") {
		return "", ErrPathTraversal
	}

	if path == "." {
		return "", ErrEmptyPath
	}

	if before, _, _ := strings.Cut(path, "/"); before == ".git" {
		return "", ErrDisallowedPath
	}

	return path, nil
}

type addTreeEntryConfig struct {
	overwriteFile bool
	overwriteDir  bool
}

// AddTreeEntryOption is a function that sets options on the addTreeEntryConfig
// struct.
type AddTreeEntryOption func(*addTreeEntryConfig)

// WithOverwriteFile allows Add to overwrite a file
func WithOverwriteFile() AddTreeEntryOption {
	return func(a *addTreeEntryConfig) {
		a.overwriteFile = true
	}
}

// WithOverwriteDirectory allows Add to overwrite a directory
func WithOverwriteDirectory() AddTreeEntryOption {
	return func(a *addTreeEntryConfig) {
		a.overwriteDir = true
	}
}

// Add takes an entry and adds it to an existing TreeEntry
// based on path.
// It works by walking down the TreeEntry's Entries, layer by layer, based on
// path. If it reaches the limit when walking down the tree, that means the
// rest of the directories path will need to be created.
// If we're able to walk all the way down the tree based on path, then we
// either add the new entry to the last subtree's entries if it doesn't yet
// exist, or optionally overwrite the entry if it already exists.
func (t *TreeEntry) Add(
	path string,
	newEntry TreeEntry,
	options ...AddTreeEntryOption,
) error {
	path, err := validateFileCreationPath(path)
	if err != nil {
		return err
	}

	var cfg addTreeEntryConfig

	for _, option := range options {
		option(&cfg)
	}

	var firstComponent string
	var parentDirs string

	secondComponent := path

	// currentTree keeps track of the current tree we are examining.
	currentTree := t

Loop:
	for {
		// A tree of ReadTreeLazily is read when the walk first reaches it.
		if err := currentTree.ensureLoaded(); err != nil {
			return err
		}

		// loop through each layer of the tree based on the directory
		// structure specified by path.
		// firstComponent is the current directory name, and secondComponent is the rest
		// of the sub directories we have yet to walk down into.
		firstComponent, secondComponent, _ = strings.Cut(secondComponent, "/")

		// look through the current tree's entries.
		for i, entry := range currentTree.Entries {
			// If the entry's Path does not match firstComponent, we don't
			// want to do anything with it.
			if entry.Path != firstComponent {
				continue
			}

			// If the entry's Path does match firstComponent, then either
			// we found the next directory to walk into, or we've
			// found the entry we want to replace.

			// We found an entry in the tree that matches the entry
			// we want to add. Replace the entry or throw an error,
			// depending on the options passed in via options
			if filepath.Join(parentDirs, entry.Path) == path {
				if (entry.Type == Tree && !cfg.overwriteDir) ||
					(entry.Type == Blob && !cfg.overwriteFile) {
					return ErrEntryExists
				}

				currentTree.OID = ""
				currentTree.Entries[i] = &newEntry

				return nil
			}

			// We found the next directory to walk into.
			currentTree.OID = ""
			currentTree = entry
			parentDirs = filepath.Join(parentDirs, entry.Path)
			continue Loop
		}

		// If secondComponent is empty, the end of the specified path has been reached.
		// The new entry is added to the current tree and the operation completes.
		if secondComponent == "" {
			currentTree.OID = ""
			currentTree.Entries = append(
				currentTree.Entries,
				&newEntry,
			)
			return nil
		}

		currentTree.Entries = append(currentTree.Entries, &TreeEntry{
			Mode: "040000",
			Type: Tree,
			Path: firstComponent,
		})

		// Empty out the OID because we've modified this tree, and will
		// need to write a new one. Write() will write a new tree object
		// if it sees that the OID is empty.
		currentTree.OID = ""
		currentTree = currentTree.Entries[len(currentTree.Entries)-1]
	}
}

// recurse is a common function to recurse down into a TreeEntry based on path,
// and take some action once we've found the entry in question.
// entryFn is called on the entry specified by path.
// treeFn is called on each tree visited during the walk.
func (t *TreeEntry) recurse(
	path string,
	entryFn func(currentTree, entry *TreeEntry, i int) error,
	treeFn func(entry *TreeEntry) error,
) error {
	var firstComponent string
	var parentDirs string
	secondComponent := path

	currentTree := t
	for {
		// A tree of ReadTreeLazily is read when the walk first reaches it.
		if err := currentTree.ensureLoaded(); err != nil {
			return err
		}

		// Loop through each layer of the tree based on the directory
		// structure specified by path.
		// firstComponent is the current directory name, and secondComponent is the rest
		// of the sub directories we have yet to walk down into.
		firstComponent, secondComponent, _ = strings.Cut(secondComponent, "/")

		// Look through the current tree's entries.
		for i, entry := range currentTree.Entries {
			if firstComponent != entry.Path {
				continue
			}

			if filepath.Join(parentDirs, entry.Path) == path {
				currentTree.OID = ""

				// Once we find the entry in question, apply the
				// function to modify the current tree and/or
				// entry.
				return entryFn(currentTree, entry, i)
			}

			if err := treeFn(currentTree); err != nil {
				return err
			}
			currentTree = entry
			parentDirs = filepath.Join(parentDirs, entry.Path)
		}

		if secondComponent == "" {
			return ErrEntryNotFound
		}
	}
}

// Delete deletes the entry of a current tree based on the path. The parent
// entry is deleted if the entry is the last child of the parent, this
// propagates up to the root node, but the root node is never deleted.
func (t *TreeEntry) Delete(
	path string,
) error {
	path, err := validateFileCreationPath(path)
	if err != nil {
		return err
	}

	entriesRemaining := 0

	if err := t.recurse(
		path,
		func(currentTree, entry *TreeEntry, i int) error {
			currentTree.Entries = append(
				currentTree.Entries[:i],
				currentTree.Entries[i+1:]...)

			entriesRemaining = len(currentTree.Entries)

			return nil
		},
		func(entry *TreeEntry) error {
			entry.OID = ""
			return nil
		},
	); err != nil {
		return err
	}

	// We check if the child was last child entry of the tree, if so, we delete the
	// parent entry too.
	//
	// We check for "." directory because filepath.Dir resolves the root dir to ".".
	if dir := filepath.Dir(path); entriesRemaining == 0 && dir != "." {
		return t.Delete(dir)
	}

	return nil
}

// Get gets an entry of a current tree based on the path.
func (t *TreeEntry) Get(
	path string,
) (*TreeEntry, error) {
	if path == "" || path == "." {
		return t, nil
	}

	var result *TreeEntry

	if err := t.recurse(path, func(currentTree, entry *TreeEntry, i int) error {
		result = entry
		return nil
	},
		func(entry *TreeEntry) error {
			return nil
		},
	); err != nil {
		return nil, err
	}

	return result, nil
}

// Modify modifies an existing TreeEntry based on a path and a function to
// modify an entry.
func (t *TreeEntry) Modify(
	path string,
	modifyEntry func(*TreeEntry) error,
) error {
	path, err := validateFileCreationPath(path)
	if err != nil {
		return err
	}

	return t.recurse(path, func(currentTree, entry *TreeEntry, _ int) error {
		return modifyEntry(entry)
	},
		func(entry *TreeEntry) error {
			entry.OID = ""
			return nil
		},
	)
}

// listEntries lists tree entries for the given treeish.
func (repo *Repo) listEntries(
	ctx context.Context,
	treeish git.Revision,
	relativePath string,
	recursive bool,
) ([]*TreeEntry, error) {
	flags := []gitcmd.Option{gitcmd.Flag{Name: "-z"}}
	if recursive {
		flags = append(flags,
			gitcmd.Flag{Name: "-r"},
			// By default, when -r is passed, tree entries will not
			// be shown. -t will cause tree entries to be shown as
			// well even when -r is passed.
			gitcmd.Flag{Name: "-t"},
		)
	}

	if relativePath == "." {
		relativePath = ""
	}

	var stderr bytes.Buffer
	cmd, err := repo.Exec(ctx, gitcmd.Command{
		Name:  "ls-tree",
		Args:  []string{fmt.Sprintf("%s:%s", treeish, relativePath)},
		Flags: flags,
	}, gitcmd.WithStderr(&stderr), gitcmd.WithSetupStdout())
	if err != nil {
		return nil, fmt.Errorf("spawning git-ls-tree: %w", err)
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	parser := NewParser(cmd, objectHash)
	var entries []*TreeEntry
	for {
		entry, err := parser.NextEntry()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, fmt.Errorf("parsing tree entry: %w", err)
		}

		entries = append(entries, entry)
	}

	if err := cmd.Wait(); err != nil {
		return nil, structerr.New("waiting for git-ls-tree: %w", err).WithMetadata("stderr", stderr.String())
	}

	return entries, nil
}

// depthByPath must be called only on cleaned paths
func depthByPath(path string) int {
	return strings.Count(path, "/") + 1
}

// treeStack is a stack data structure used by walk() to do a breadth-first walk
// of the outputof ls-tree -r.
type treeStack []*TreeEntry

func (t treeStack) peek() *TreeEntry {
	if len(t) == 0 {
		return nil
	}

	return t[len(t)-1]
}

func (t *treeStack) push(e *TreeEntry) {
	*t = append(*t, e)
}

func (t *treeStack) pop() *TreeEntry {
	e := t.peek()
	if e == nil {
		return nil
	}

	*t = (*t)[:len(*t)-1]

	return e
}

// populate scans through the output of ls-tree -r, and constructs a TreeEntry
// object.
func (t *TreeEntry) populate(
	ctx context.Context,
	repo *Repo,
) error {
	if t.OID == "" {
		return errors.New("oid is empty")
	}

	t.Entries = nil

	entries, err := repo.listEntries(
		ctx,
		git.Revision(t.OID),
		"",
		true,
	)
	if err != nil {
		return err
	}

	stack := treeStack{t}

	// The output of ls-tree -r is the following:
	// a1
	// dir1/file2
	// dir2/file3
	// f2
	// f3
	// Whenever we see a tree, push it onto the stack since we will need to
	// start adding to that tree's entries.
	// When we encounter an entry that has a lower depth than the previous
	// entry, we know that we need to pop the tree entry off to get back to the
	// parent tree.
	for _, entry := range entries {
		if levelsToPop := len(stack) - depthByPath(entry.Path); levelsToPop > 0 {
			for i := 0; i < levelsToPop; i++ {
				stack.pop()
			}
		}

		entry.Path = filepath.Base(entry.Path)
		stack.peek().Entries = append(
			stack.peek().Entries,
			entry,
		)

		if entry.Type == Tree {
			stack.push(entry)
		}
	}

	if err != nil {
		return fmt.Errorf("listing entries: %w", err)
	}

	return nil
}

func (t *TreeEntry) walk(dirPath string, callback func(path string, entry *TreeEntry) error) error {
	nextDirPath := filepath.Join(dirPath, t.Path)
	if err := callback(dirPath, t); err != nil {
		return err
	}

	if err := t.ensureLoaded(); err != nil {
		return err
	}
	for _, entry := range t.Entries {
		if err := entry.walk(nextDirPath, callback); err != nil {
			return err
		}
	}

	return nil
}

// Walk will walk the whole tree structure and call callback on every entry of
// the tree in a depth-first like fashion.
func (t *TreeEntry) Walk(callback func(path string, entry *TreeEntry) error) error {
	if err := t.ensureLoaded(); err != nil {
		return err
	}
	for _, e := range t.Entries {
		if err := e.walk(t.Path, callback); err != nil {
			return err
		}
	}

	return nil
}

// Write does a depth first walk, writing new trees when the OID of the
// TreeEntry is empty.
func (t *TreeEntry) Write(
	ctx context.Context,
	repo *Repo,
) error {
	var err error

	if t.OID != "" {
		return nil
	}

	for _, e := range t.Entries {
		if e.Type == Tree && e.OID == "" {
			if err := e.Write(ctx, repo); err != nil {
				return err
			}
		}
	}

	treeOID, err := repo.writeEntries(ctx, t.Entries)
	if err != nil {
		return fmt.Errorf("writing tree: %w", err)
	}

	t.OID = treeOID
	return nil
}

// treeContent returns the content of the tree object of entries, which it sorts in git's order.
func treeContent(entries []*TreeEntry) (*bytes.Buffer, error) {
	var tree bytes.Buffer

	sort.Stable(TreeEntriesByPath(entries))

	for _, entry := range entries {
		mode := strings.TrimPrefix(entry.Mode, "0")
		formattedEntry := fmt.Sprintf("%s %s\000", mode, entry.Path)

		oidBytes, err := entry.OID.Bytes()
		if err != nil {
			return nil, err
		}

		if _, err := tree.WriteString(formattedEntry); err != nil {
			return nil, err
		}

		if _, err := tree.Write(oidBytes); err != nil {
			return nil, err
		}
	}

	return &tree, nil
}

// writeTreeEntries writes a new tree object from a slice of entries. This function does not verify whether OIDs
// referred to by tree entries actually exist in the repository.
func (repo *Repo) writeEntries(ctx context.Context, entries []*TreeEntry) (git.ObjectID, error) {
	tree, err := treeContent(entries)
	if err != nil {
		return "", err
	}

	options := []gitcmd.Option{
		gitcmd.ValueFlag{Name: "-t", Value: "tree"},
		gitcmd.Flag{Name: "-w"},
		gitcmd.Flag{Name: "--stdin"},
	}

	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "hash-object",
			Flags: options,
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
		gitcmd.WithStdin(tree),
	); err != nil {
		return "", structerr.New("%w", err).WithMetadata("stderr", stderr.String())
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", fmt.Errorf("detecting object hash: %w", err)
	}

	treeOID, err := objectHash.FromHex(text.ChompBytes(stdout.Bytes()))
	if err != nil {
		return "", err
	}

	return treeOID, nil
}

// readTreeConfig is configuration that can be passed to ReadTree.
type readTreeConfig struct {
	recursive bool
	// relativePath is the relative path at which listing of entries should be started.
	relativePath string
}

// ReadTreeOption is an option that modifies the behavior of ReadTree()
type ReadTreeOption func(c *readTreeConfig)

// WithRecursive returns all entries recursively, but "flattened" in the sense
// that all subtrees and their entries get returned as Entries, each entry with
// its full path.
func WithRecursive() ReadTreeOption {
	return func(c *readTreeConfig) {
		c.recursive = true
	}
}

// WithRelativePath will cause ReadTree to return a tree at the relative path.
func WithRelativePath(relativePath string) ReadTreeOption {
	return func(c *readTreeConfig) {
		c.relativePath = relativePath
	}
}

// ReadTree gets a tree object with all of the direct children populated.
// Walk can be called to populate every level of the tree.
func (repo *Repo) ReadTree(ctx context.Context, treeish git.Revision, options ...ReadTreeOption) (*TreeEntry, error) {
	var c readTreeConfig

	for _, opt := range options {
		opt(&c)
	}

	if c.relativePath == "." {
		c.relativePath = ""
	}

	rev := git.Revision(string(treeish) + ":" + c.relativePath)

	treeOID, err := repo.ResolveRevision(ctx, rev)
	if err != nil {
		return nil, fmt.Errorf("getting revision: %w", err)
	}

	objectReader, cancel, err := repo.catfileCache.ObjectInfoReader(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("get catfile reader: %w", err)
	}
	defer cancel()

	// Perform a preliminary check on the revision to ensure it's treeish.
	obj, err := objectReader.Info(ctx, treeOID.Revision())
	if err != nil {
		// If the resolved revision does not exist, the repository is either corrupt or the revision
		// refers to the commit ID of a Git submodule. We assume it is the latter if a relative path
		// is present. Reading tree entries from a submodule is not supported and consequently an
		// error is returned.
		if errors.As(err, &catfile.NotFoundError{}) && c.relativePath != "" {
			return nil, fmt.Errorf("reading resolved revision: %w", ErrTreeNotExist)
		}

		return nil, fmt.Errorf("check object type: %w", err)
	}
	if obj.Type != "tree" {
		return nil, ErrNotTreeish
	}

	rootEntry := &TreeEntry{
		OID:  treeOID,
		Type: Tree,
		Mode: "040000",
	}

	if c.recursive {
		if err := rootEntry.populate(ctx, repo); err != nil {
			return nil, err
		}
	} else {
		if rootEntry.Entries, err = repo.listEntries(
			ctx,
			treeish,
			c.relativePath,
			c.recursive,
		); err != nil {
			return nil, fmt.Errorf("listEntries: %w", err)
		}
	}

	return rootEntry, nil
}

// ReadTreeLazily returns the tree treeOID with its direct entries read through reader, the object
// reader of the request. Its subtrees are read only when an operation (Add, Delete, Get, Modify,
// Walk) descends into them, through the same reader, so a request that changes a few paths reads
// a few trees instead of the whole tree (PLAN9, WP96). The entries are those ReadTree with
// WithRecursive gives, for every tree that is read. reader must stay open while the tree is used.
func (repo *Repo) ReadTreeLazily(ctx context.Context, reader catfile.ObjectReader, treeOID git.ObjectID) (*TreeEntry, error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	obj, err := reader.Info(ctx, treeOID.Revision())
	if err != nil {
		return nil, fmt.Errorf("check object type: %w", err)
	}
	if obj.Type != "tree" {
		return nil, ErrNotTreeish
	}

	root := &TreeEntry{
		OID:    treeOID,
		Type:   Tree,
		Mode:   "040000",
		loader: &treeLoader{ctx: ctx, reader: reader, objectHash: objectHash},
	}
	if err := root.ensureLoaded(); err != nil {
		return nil, err
	}

	return root, nil
}

// TreeWriter writes the tree objects of one request through a single `git hash-object -t tree -w
// --stdin-paths` process, started on the first write: today's tree writer, with the same input and
// the same format checks, without a process per tree (PLAN9, WP96). The content of each tree is
// written to a file of the storage's temporary directory (created exclusively, mode 0600, removed
// after the write, whatever happened), whose path the process reads. A write that fails stops the
// process and makes every later write fail: a tree the process refused ends the request, as
// today. Close must be called on every path.
type TreeWriter struct {
	repo       *Repo
	ctx        context.Context
	tempDir    string
	objectHash git.ObjectHash

	cmd    *command.Command
	stdout *bufio.Reader
	stderr bytes.Buffer
	cancel context.CancelFunc
	err    error
}

var errTreeWriterClosed = errors.New("tree writer is closed")

// NewTreeWriter returns the tree writer of a request on repo. Its process runs under ctx.
func (repo *Repo) NewTreeWriter(ctx context.Context) (*TreeWriter, error) {
	tempDir, err := repo.StorageTempDir()
	if err != nil {
		return nil, fmt.Errorf("getting temporary directory: %w", err)
	}
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	return &TreeWriter{repo: repo, ctx: ctx, tempDir: tempDir, objectHash: objectHash}, nil
}

// write writes the tree of entries and returns its object ID.
func (w *TreeWriter) write(entries []*TreeEntry) (_ git.ObjectID, returnedErr error) {
	if w.err != nil {
		return "", w.err
	}
	defer func() {
		if returnedErr != nil {
			w.err = returnedErr
			w.stop()
		}
	}()

	content, err := treeContent(entries)
	if err != nil {
		return "", err
	}

	// os.CreateTemp opens the file with O_CREATE|O_EXCL and mode 0600.
	file, err := os.CreateTemp(w.tempDir, "tree-object-*")
	if err != nil {
		return "", fmt.Errorf("creating tree object file: %w", err)
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) && returnedErr == nil {
			returnedErr = fmt.Errorf("removing tree object file: %w", err)
		}
	}()
	if _, err := file.Write(content.Bytes()); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("writing tree object file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("closing tree object file: %w", err)
	}

	if w.cmd == nil {
		if err := w.start(); err != nil {
			return "", err
		}
	}

	if _, err := fmt.Fprintf(w.cmd.Stdin(), "%s\n", file.Name()); err != nil {
		return "", w.processError(fmt.Errorf("sending tree object path: %w", err))
	}
	line, err := w.stdout.ReadString('\n')
	if err != nil {
		return "", w.processError(fmt.Errorf("reading tree object ID: %w", err))
	}

	oid, err := w.objectHash.FromHex(strings.TrimSuffix(line, "\n"))
	if err != nil {
		return "", fmt.Errorf("parsing tree object ID: %w", err)
	}
	return oid, nil
}

// start starts the hash-object process.
func (w *TreeWriter) start() error {
	processCtx, cancel := context.WithCancel(w.ctx)
	cmd, err := w.repo.Exec(processCtx,
		gitcmd.Command{
			Name: "hash-object",
			Flags: []gitcmd.Option{
				gitcmd.ValueFlag{Name: "-t", Value: "tree"},
				gitcmd.Flag{Name: "-w"},
				gitcmd.Flag{Name: "--stdin-paths"},
			},
		},
		gitcmd.WithSetupStdin(),
		gitcmd.WithSetupStdout(),
		gitcmd.WithStderr(&w.stderr),
	)
	if err != nil {
		cancel()
		return fmt.Errorf("starting hash-object: %w", err)
	}

	w.cmd = cmd
	w.stdout = bufio.NewReader(cmd)
	w.cancel = cancel
	return nil
}

// processError reaps the process, which stopped answering, and returns why: its exit status with
// what it printed on stderr, as today's writer reports a refused tree, or err if it exited
// cleanly. Wait closes its stdin first.
func (w *TreeWriter) processError(err error) error {
	waitErr := w.cmd.Wait()
	w.cmd = nil
	w.cancel()
	if waitErr != nil {
		err = waitErr
	}
	return structerr.New("%w", err).WithMetadata("stderr", w.stderr.String())
}

// stop kills and reaps the process, if any.
func (w *TreeWriter) stop() {
	if w.cmd == nil {
		return
	}
	w.cancel()
	// The process was killed, so its exit status carries no information.
	_ = w.cmd.Wait()
	w.cmd = nil
}

// Close ends the process: it reads the end of its input and exits, so every tree it wrote is in the
// object directory when Close returns. Close is idempotent, later calls return nil, and it is safe
// after a failed write, whose process is already reaped. Writes fail after Close.
func (w *TreeWriter) Close() error {
	if w.err == nil {
		w.err = errTreeWriterClosed
	}
	if w.cmd == nil {
		return nil
	}
	defer w.cancel()

	cmd := w.cmd
	w.cmd = nil
	// Wait closes the process's stdin first.
	if err := cmd.Wait(); err != nil {
		return structerr.New("waiting for tree writer: %w", err).WithMetadata("stderr", w.stderr.String())
	}
	return nil
}

// WriteWith is Write through the request's tree writer: the same trees, written depth first when
// their OID is empty, by one process.
func (t *TreeEntry) WriteWith(w *TreeWriter) error {
	if t.OID != "" {
		return nil
	}

	for _, e := range t.Entries {
		if e.Type == Tree && e.OID == "" {
			if err := e.WriteWith(w); err != nil {
				return err
			}
		}
	}

	treeOID, err := w.write(t.Entries)
	if err != nil {
		return fmt.Errorf("writing tree: %w", err)
	}

	t.OID = treeOID
	return nil
}
