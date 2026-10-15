package derived

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/catfile"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

const (
	// kindLastMod is the kind of the last commit that changed a path, following
	// Git's default history simplification (git log -- path).
	kindLastMod = "lastmod"
	// kindLastModFirstParent is the last commit that changed a path when only
	// first parents are followed (git log --first-parent -- path).
	kindLastModFirstParent = "lastmod-firstparent"
	// kindBlameOrigin is the commit whose blame of a path is the blame of the path
	// at the commit it was computed for. It follows the parents that git blame
	// follows, which compare blob object IDs and file types but ignore
	// permissions. Earlier versions stored origins under "blame-origin" with a
	// comparison that ignored file types, so those entries can be wrong and are
	// never read.
	kindBlameOrigin = "blame-origin-typed"
	// kindBlame is the parsed blame of a path at its blame origin.
	kindBlame = "blame"

	// treeMode is the file mode of a tree, which is the mode of the root directory.
	treeMode int32 = 0o40000

	// maxMemoizedNodes bounds the number of commits whose objects a Deriver keeps
	// in memory during a walk. The memo only saves reads, so it is dropped when full.
	maxMemoizedNodes = 1 << 16
)

// objectIDRegexp matches a SHA-1 or SHA-256 object ID in lowercase hex.
var objectIDRegexp = regexp.MustCompile(`\A([0-9a-f]{40}|[0-9a-f]{64})\z`)

// fingerprint describes the state of a path in one commit: the mode and object
// ID of the path's tree entry. The zero fingerprint means that the path does not
// exist in the commit.
type fingerprint struct {
	// mode is the canonical mode of the entry, as Git normalizes it when it reads
	// a tree (see canonMode).
	mode uint32
	// oid is the object ID of the entry.
	oid string
}

func (fp fingerprint) exists() bool { return fp.oid != "" }

func (fp fingerprint) isTree() bool { return fp.mode == uint32(treeMode) }

// fileType returns the type bits (S_IFMT) of the entry's mode: regular file,
// symlink, gitlink or tree. It is zero for an absent path.
func (fp fingerprint) fileType() uint32 { return fp.mode & 0o170000 }

// comparison decides whether a commit agrees with one of its parents on a path,
// that is, whether the commit did not change the path relative to that parent.
type comparison func(ctx context.Context, parent, current fingerprint) (bool, error)

// sameBlobs is the comparison of git blame, which passes the whole blame on to a
// parent whose entry has the same object ID and the same file type. It ignores
// permission changes (100644 and 100755), which Git's diff reports as a
// modification ("M"), but not type changes such as a symlink that becomes a
// regular file, which Git's diff reports as "T" and for which find_origin finds no
// origin in that parent.
func sameBlobs(_ context.Context, parent, current fingerprint) (bool, error) {
	return parent.oid == current.oid && parent.fileType() == current.fileType(), nil
}

// sameEntries is the TREESAME relation of git log -- path. Git compares the
// commits with a recursive diff limited to path, which reports only files
// (blobs, symlinks and submodules) and never trees. Two entries therefore agree
// when they hold the same files with the same modes. Equal entries trivially do;
// otherwise only trees, including absent ones, can still agree, for example
// an absent path and an empty tree, or trees that differ only in empty subtrees.
func (d *Deriver) sameEntries(ctx context.Context, parent, current fingerprint) (bool, error) {
	if parent == current {
		return true, nil
	}
	if (parent.exists() && !parent.isTree()) || (current.exists() && !current.isTree()) {
		// A file differs from anything but an equal file: from an absent path and a
		// tree by being a file that the other side lacks.
		return false, nil
	}

	return d.sameFiles(ctx, parent.oid, current.oid)
}

// sameFiles reports whether the trees a and b, either of which may be empty to
// stand for a missing tree, hold the same files at the same paths with the same
// modes. Subtrees are compared through an explicit stack, and only subtrees whose
// object IDs differ are read, as Git's diff does.
func (d *Deriver) sameFiles(ctx context.Context, a, b string) (bool, error) {
	type pair struct{ a, b string }

	for stack := []pair{{a: a, b: b}}; len(stack) > 0; {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if p.a == p.b {
			continue
		}

		entriesA, err := d.readTree(ctx, p.a)
		if err != nil {
			return false, err
		}
		entriesB, err := d.readTree(ctx, p.b)
		if err != nil {
			return false, err
		}

		for name, entryA := range entriesA {
			entryB, inB := entriesB[name]
			switch {
			case inB && entryA == entryB:
				continue
			case !entryA.isTree() || (inB && !entryB.isTree()):
				return false, nil
			}
			stack = append(stack, pair{a: entryA.oid, b: entryB.oid})
		}
		for name, entryB := range entriesB {
			if _, inA := entriesA[name]; inA {
				continue
			}
			if !entryB.isTree() {
				return false, nil
			}
			stack = append(stack, pair{b: entryB.oid})
		}
	}

	return true, nil
}

// readTree returns the entries of the tree with the given object ID by name. The
// empty ID and the empty tree have no entries.
func (d *Deriver) readTree(ctx context.Context, oid string) (map[string]fingerprint, error) {
	if oid == "" || isEmptyTree(oid) {
		return nil, nil
	}

	object, err := d.reader.Object(ctx, git.Revision(oid))
	if err != nil {
		return nil, fmt.Errorf("reading tree %s: %w", oid, err)
	}

	entries := make(map[string]fingerprint)
	if err := parseTree(object, func(name string, fp fingerprint) bool {
		entries[name] = fp
		return true
	}); err != nil {
		return nil, fmt.Errorf("reading tree %s: %w", oid, err)
	}
	if object.Type != "tree" {
		return nil, fmt.Errorf("object %s is a %s, not a tree", oid, object.Type)
	}

	return entries, nil
}

// isEmptyTree reports whether oid is the empty tree of either object format.
func isEmptyTree(oid string) bool {
	return oid == git.ObjectHashSHA1.EmptyTreeOID.String() || oid == git.ObjectHashSHA256.EmptyTreeOID.String()
}

// canonMode normalizes a tree entry mode the way Git does when it reads trees
// (canon_mode), so that, for example, an old 100664 file equals a 100644 one.
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
		return 0o040000
	default:
		return 0o160000
	}
}

// node is what the walk needs to know about one commit.
type node struct {
	commit *catfile.Commit
	fp     fingerprint
}

type memoKey struct {
	commit git.ObjectID
	path   string
}

// Deriver computes derived data for one repository within one request. It is
// not safe for concurrent use. A Deriver reads objects through the given object
// reader and, when caching is enabled, persists a bounded number of the computed
// values (see lastMod) in the repository's derived-data store so that later
// queries are answered with few or no reads and without running Git.
type Deriver struct {
	logger log.Logger
	repo   *localrepo.Repo
	reader catfile.ObjectContentReader
	store  *Store

	// memo holds the commits and tree entries that were read during the walks of
	// this Deriver, so that a commit which becomes the current one of a walk, or
	// which is shared by several walks, is not read again.
	memo map[memoKey]node

	commitsComputed atomic.Int64
	blameRuns       atomic.Int64
}

// NewDeriver returns a Deriver for repo. When cache is false the store is nil:
// nothing is read from or written to the repository's derived-data store.
func NewDeriver(ctx context.Context, logger log.Logger, repo *localrepo.Repo, reader catfile.ObjectContentReader, cache bool) (*Deriver, error) {
	d := &Deriver{logger: logger, repo: repo, reader: reader, memo: make(map[memoKey]node)}

	if cache {
		repoPath, err := repo.Path(ctx)
		if err != nil {
			return nil, fmt.Errorf("getting repository path: %w", err)
		}
		d.store = NewStore(repoPath)
	}

	return d, nil
}

// CommitsComputed returns how many commits had their last-modified value
// computed (rather than read from the store) by this Deriver.
func (d *Deriver) CommitsComputed() int64 { return d.commitsComputed.Load() }

// BlameRuns returns how many times this Deriver ran git blame (rather than
// reading the blame from the store).
func (d *Deriver) BlameRuns() int64 { return d.blameRuns.Load() }

// SupportsPath reports whether path can be answered by the derived-data
// queries, that is, whether git log -- path looks up exactly the path. Paths
// that Git interprets as pathspec patterns (globs, or the ":" magic prefix) are
// not supported. Neither are paths that Git normalizes before matching, such as
// paths with a trailing slash, empty or "." components, or ".." components, nor
// paths with a NUL byte. Git matches a path ending in "/." as a directory only, so
// it can never match a file. Callers must use Git for all of these.
func SupportsPath(path string) bool {
	switch {
	case path == "", path == "/":
		// The empty path and "/" both name the repository root.
		return true
	case strings.ContainsAny(path, "*?[\\\x00"), strings.HasPrefix(path, ":"), strings.HasPrefix(path, "/"):
		return false
	case path == "..", strings.HasPrefix(path, "../"):
		return false
	}

	return pathpkg.Clean(path) == path
}

// SupportsRevision reports whether revision names exactly one commit, so that
// the derived-data queries answer for the same commit that git log would. Ranges,
// exclusions (a leading "^") and revisions that select several commits are not
// supported, and callers must use Git for them.
func SupportsRevision(revision string) bool {
	if strings.HasPrefix(revision, "^") {
		return false
	}
	for _, syntax := range []string{"..", "^@", "^!", "^-"} {
		if strings.Contains(revision, syntax) {
			return false
		}
	}
	return true
}

// normalizePath maps the empty path and "/" to the repository root and cleans
// every other path, so that equivalent paths share cache entries. Callers that
// accept arbitrary paths must check SupportsPath or validate the path first.
func normalizePath(path string) string {
	if path == "" || path == "/" {
		return "."
	}
	return pathpkg.Clean(path)
}

// LastMod returns the last commit that changed path as of commit, equal to the
// first commit shown by `git log -- path` started at commit. It returns the
// empty object ID when path does not exist in the history of commit.
func (d *Deriver) LastMod(ctx context.Context, commit git.ObjectID, path string) (git.ObjectID, error) {
	return d.lastMod(ctx, kindLastMod, d.sameEntries, false, commit, normalizePath(path))
}

// BlameOrigin returns the commit that git blame at commit attributes the blame of
// path to, so that the blame at commit equals the blame at the origin. git blame
// passes the whole blame of a path to the first parent whose version of the path
// has the same blob object and file type, and it ignores permission changes. The
// origin is therefore the end of that chain, which is different from LastMod when
// only the permissions of a path changed. The empty object ID means that path does
// not exist at commit.
func (d *Deriver) BlameOrigin(ctx context.Context, commit git.ObjectID, path string) (git.ObjectID, error) {
	path = normalizePath(path)

	exists, err := d.PathExists(ctx, commit, path)
	if err != nil {
		return "", err
	}
	if !exists {
		// Nothing to blame. Walking would only follow the absent path to the root.
		return "", nil
	}

	return d.lastMod(ctx, kindBlameOrigin, sameBlobs, false, commit, path)
}

// PathExists reports whether path exists at commit. The root directory always
// exists. The commit is read through the Deriver's memo, so a walk that starts at
// commit afterwards does not read it again.
func (d *Deriver) PathExists(ctx context.Context, commit git.ObjectID, path string) (bool, error) {
	path = normalizePath(path)

	nodes, err := d.nodes(ctx, path, commit)
	if err != nil {
		return false, err
	}

	return nodes[commit].fp.exists(), nil
}

// lastMod walks the history from start with the chain walk of kind. Each commit's
// value is either the commit itself, when no parent agrees with it on path, or the
// value of its first agreeing parent, which is the only parent Git's history
// simplification follows. With firstParentOnly the only parent that is considered
// is the first one. See chainWalk for the storage rules.
func (d *Deriver) lastMod(
	ctx context.Context,
	kind string,
	same comparison,
	firstParentOnly bool,
	start git.ObjectID,
	path string,
) (git.ObjectID, error) {
	return chainWalk{
		kind:   kind,
		key:    func(commit git.ObjectID) string { return Key(kind, commit, path) },
		store:  d.store,
		logger: d.logger,
		step: func(ctx context.Context, commit git.ObjectID) (git.ObjectID, git.ObjectID, error) {
			return d.lastModStep(ctx, path, same, firstParentOnly, commit)
		},
		onCompute: func() { d.commitsComputed.Add(1) },
	}.run(ctx, start)
}

// lastModStep returns the link of commit in the last-modified chain of path: the
// first agreeing parent that the walk continues at, or else the commit itself as
// the result, or the empty result when the root commit agrees with an absent path.
func (d *Deriver) lastModStep(
	ctx context.Context,
	path string,
	same comparison,
	firstParentOnly bool,
	current git.ObjectID,
) (git.ObjectID, git.ObjectID, error) {
	nodes, err := d.nodes(ctx, path, current)
	if err != nil {
		return "", "", err
	}
	cur := nodes[current]

	parents := cur.commit.GetParentIds()
	if len(parents) == 0 {
		// A root commit is compared with an absent path, as Git compares it with the
		// empty tree: it changed path unless it lacks the path or, for git log, the
		// path holds no files at all (an empty tree).
		unchanged, err := same(ctx, fingerprint{}, cur.fp)
		if err != nil {
			return "", "", err
		}
		if unchanged {
			return "", "", nil
		}
		return "", current, nil
	}
	if firstParentOnly {
		parents = parents[:1]
	}

	parentIDs := make([]git.ObjectID, 0, len(parents))
	for _, parentID := range parents {
		parentIDs = append(parentIDs, git.ObjectID(parentID))
	}

	parentNodes, err := d.nodes(ctx, path, parentIDs...)
	if err != nil {
		return "", "", err
	}

	for _, parentID := range parentIDs {
		unchanged, err := same(ctx, parentNodes[parentID].fp, cur.fp)
		if err != nil {
			return "", "", err
		}
		if unchanged {
			return parentID, "", nil
		}
	}

	return "", current, nil
}

// nodes returns the commit and the state of path for every commit in ids. The
// values that the Deriver has not read yet are read in one batch.
func (d *Deriver) nodes(ctx context.Context, path string, ids ...git.ObjectID) (map[git.ObjectID]node, error) {
	result := make(map[git.ObjectID]node, len(ids))

	var missing []git.ObjectID
	for _, id := range ids {
		if _, ok := result[id]; ok {
			continue
		}
		if n, ok := d.memo[memoKey{commit: id, path: path}]; ok {
			result[id] = n
			continue
		}

		missing = append(missing, id)
		result[id] = node{}
	}

	if len(missing) == 0 {
		return result, nil
	}

	fetched, err := d.fetch(ctx, path, missing)
	if err != nil {
		return nil, err
	}

	if len(d.memo) >= maxMemoizedNodes {
		d.memo = make(map[memoKey]node)
	}

	for id, n := range fetched {
		result[id] = n
		d.memo[memoKey{commit: id, path: path}] = n
	}

	return result, nil
}

// fetch reads the commits and the state of path in them. All requests are sent
// in one batch through the object queue, so that a walk step costs a single
// round trip to git cat-file instead of one per object.
func (d *Deriver) fetch(ctx context.Context, path string, ids []git.ObjectID) (map[git.ObjectID]node, error) {
	queue, done, err := d.reader.Queue(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating object queue: %w", err)
	}
	defer done()

	// The state of a path that is not the root is found in the tree of its directory,
	// which git resolves from "<commit>:<directory>". The root is the commit's own tree.
	inTree := path != "."
	dir := ""
	if inTree {
		if dir = pathpkg.Dir(path); dir == "." {
			dir = ""
		}
	}

	for _, id := range ids {
		if err := queue.RequestObject(ctx, id.Revision()+"^{commit}"); err != nil {
			return nil, fmt.Errorf("requesting commit %s: %w", id, err)
		}
	}
	if inTree {
		for _, id := range ids {
			if err := queue.RequestObject(ctx, git.Revision(id.String()+":"+dir)); err != nil {
				return nil, fmt.Errorf("requesting tree of %s: %w", id, err)
			}
		}
	}

	if err := queue.Flush(ctx); err != nil {
		return nil, fmt.Errorf("flushing object queue: %w", err)
	}

	commits := make(map[git.ObjectID]*catfile.Commit, len(ids))
	for _, id := range ids {
		object, err := queue.ReadObject(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading commit %s: %w", id, err)
		}

		commit, err := catfile.NewParser().ParseCommit(object)
		if err != nil {
			return nil, fmt.Errorf("parsing commit %s: %w", id, err)
		}
		if _, err := io.Copy(io.Discard, object); err != nil {
			return nil, fmt.Errorf("discarding commit %s: %w", id, err)
		}

		commits[id] = commit
	}

	result := make(map[git.ObjectID]node, len(ids))
	for _, id := range ids {
		n := node{commit: commits[id]}

		switch {
		case !inTree:
			n.fp = fingerprint{mode: uint32(treeMode), oid: n.commit.GetTreeId()}
		default:
			object, err := queue.ReadObject(ctx)
			var notFound catfile.NotFoundError
			switch {
			case errors.As(err, &notFound):
				// The directory does not exist, so neither does the path.
			case err != nil:
				return nil, fmt.Errorf("reading tree of %s: %w", id, err)
			default:
				n.fp, err = entryFingerprint(object, pathpkg.Base(path))
				if err != nil {
					return nil, fmt.Errorf("reading %s in %s: %w", path, id, err)
				}
			}
		}

		result[id] = n
	}

	return result, nil
}

// entryFingerprint returns the fingerprint of the entry called name in the tree
// object. The empty fingerprint is returned when the object is not a tree or has no
// such entry, which is how a path that is a file in a parent directory behaves.
func entryFingerprint(object *catfile.Object, name string) (fingerprint, error) {
	var result fingerprint
	if err := parseTree(object, func(entryName string, fp fingerprint) bool {
		if entryName != name {
			return true
		}
		result = fp
		return false
	}); err != nil {
		return fingerprint{}, err
	}

	return result, nil
}

// parseTree reads the whole object and, when it is a tree, calls fn for its
// entries in order until fn returns false. Objects that are not trees have no
// entries.
func parseTree(object *catfile.Object, fn func(name string, fp fingerprint) bool) error {
	data, err := io.ReadAll(object)
	if err != nil {
		return fmt.Errorf("reading tree: %w", err)
	}

	if object.Type != "tree" {
		return nil
	}

	objectHash, err := git.ObjectHashByFormat(object.Format)
	if err != nil {
		return fmt.Errorf("looking up object hash: %w", err)
	}
	hashSize := objectHash.Hash().Size()

	// A tree is a sequence of "<mode> <name>\x00<raw object ID>" entries.
	for len(data) > 0 {
		space := bytes.IndexByte(data, ' ')
		if space < 0 {
			return errors.New("malformed tree: missing mode")
		}
		mode, err := strconv.ParseUint(string(data[:space]), 8, 32)
		if err != nil {
			return fmt.Errorf("malformed tree: invalid mode: %w", err)
		}
		data = data[space+1:]

		nul := bytes.IndexByte(data, 0)
		if nul < 0 {
			return errors.New("malformed tree: missing name")
		}
		entryName := data[:nul]
		data = data[nul+1:]

		if len(data) < hashSize {
			return errors.New("malformed tree: truncated object ID")
		}
		oid := hex.EncodeToString(data[:hashSize])
		data = data[hashSize:]

		if !fn(string(entryName), fingerprint{mode: canonMode(mode), oid: oid}) {
			return nil
		}
	}

	return nil
}

// PathHistory calls fn for every commit that changed path, newest first,
// following first parents, as `git log --first-parent -- path` does. It stops
// after limit commits or when path no longer exists. Every commit is passed to
// fn as read by catfile.
func (d *Deriver) PathHistory(ctx context.Context, start git.ObjectID, path string, limit int, fn func(*catfile.Commit) error) error {
	return d.PathHistoryFiltered(ctx, start, path, limit, nil, fn)
}

// UnchangedFunc reports whether it is known, without reading trees, that commit did
// not change path relative to its first parent, in the sense of
// `git log --first-parent -- path`. The path is normalized as the Deriver normalizes
// it: "." is the root. It is a pre-filter only: it must report false whenever it does
// not know, and it must never compute what it does not have.
type UnchangedFunc func(ctx context.Context, commit git.ObjectID, path string) (bool, error)

// PathHistoryFiltered is PathHistory with a pre-filter. A commit whose link in the
// first-parent chain of path is computed, rather than read from the store, is first
// passed to unchanged: when unchanged proves that the commit did not change path, the
// walk continues at its first parent without reading the path's trees, and otherwise
// the commit is compared as PathHistory compares it. The commits reported, and the
// values stored, are the ones of PathHistory. A nil unchanged filters nothing.
func (d *Deriver) PathHistoryFiltered(
	ctx context.Context,
	start git.ObjectID,
	path string,
	limit int,
	unchanged UnchangedFunc,
	fn func(*catfile.Commit) error,
) error {
	path = normalizePath(path)

	current, err := d.lastModFirstParent(ctx, start, path, unchanged)
	if err != nil {
		return err
	}

	for n := 0; current != "" && n < limit; n++ {
		commit, err := catfile.GetCommit(ctx, d.reader, current.Revision())
		if err != nil {
			return fmt.Errorf("reading commit %s: %w", current, err)
		}

		if err := fn(commit); err != nil {
			return err
		}

		// The next value is only needed when another commit is returned, so the walk
		// must not start before it is known that the limit allows one.
		if n+1 >= limit {
			break
		}

		parents := commit.GetParentIds()
		if len(parents) == 0 {
			break
		}

		current, err = d.lastModFirstParent(ctx, git.ObjectID(parents[0]), path, unchanged)
		if err != nil {
			return err
		}
	}

	return nil
}

// lastModFirstParent is the lastmod-firstparent chain walk of path from start. The
// chain values do not depend on unchanged, which only saves the reads of the commits
// that it proves unchanged, so the values are shared with the unfiltered walk.
func (d *Deriver) lastModFirstParent(ctx context.Context, start git.ObjectID, path string, unchanged UnchangedFunc) (git.ObjectID, error) {
	if unchanged == nil {
		return d.lastMod(ctx, kindLastModFirstParent, d.sameEntries, true, start, path)
	}

	return chainWalk{
		kind:   kindLastModFirstParent,
		key:    func(commit git.ObjectID) string { return Key(kindLastModFirstParent, commit, path) },
		store:  d.store,
		logger: d.logger,
		step: func(ctx context.Context, commit git.ObjectID) (git.ObjectID, git.ObjectID, error) {
			proven, err := unchanged(ctx, commit, path)
			if err != nil {
				return "", "", err
			}
			if !proven {
				return d.lastModStep(ctx, path, d.sameEntries, true, commit)
			}
			return d.skipUnchanged(ctx, path, commit)
		},
		onCompute: func() { d.commitsComputed.Add(1) },
	}.run(ctx, start)
}

// skipUnchanged returns the link of a commit that is known not to have changed path
// relative to its first parent: the walk continues at the first parent, and a root
// commit, which agrees with an absent path, ends it with the empty result, as
// lastModStep decides for such commits. Only the commit object is read.
func (d *Deriver) skipUnchanged(ctx context.Context, path string, current git.ObjectID) (git.ObjectID, git.ObjectID, error) {
	var parents []string
	if n, ok := d.memo[memoKey{commit: current, path: path}]; ok {
		parents = n.commit.GetParentIds()
	} else {
		commit, err := catfile.GetCommit(ctx, d.reader, current.Revision()+"^{commit}")
		if err != nil {
			return "", "", fmt.Errorf("reading commit %s: %w", current, err)
		}
		parents = commit.GetParentIds()
	}

	if len(parents) == 0 {
		return "", "", nil
	}
	return git.ObjectID(parents[0]), "", nil
}

// LastCommit returns the commit that last changed path as of revision, like
// `git log -1 -- path`. It returns catfile.NotFoundError when the revision does
// not exist or path does not exist in its history.
func (d *Deriver) LastCommit(ctx context.Context, revision git.Revision, path string) (*catfile.Commit, error) {
	commitID, err := d.resolveCommit(ctx, revision)
	if err != nil {
		return nil, err
	}

	return d.LastCommitOf(ctx, commitID, path)
}

// LastCommitOf is LastCommit for a commit that has already been resolved. It
// returns catfile.NotFoundError when path does not exist in the history of commit.
func (d *Deriver) LastCommitOf(ctx context.Context, commit git.ObjectID, path string) (*catfile.Commit, error) {
	lastMod, err := d.LastMod(ctx, commit, path)
	if err != nil {
		return nil, err
	}
	if lastMod == "" {
		return nil, catfile.NotFoundError{Revision: fmt.Sprintf("%s:%s", commit, path)}
	}

	return catfile.GetCommit(ctx, d.reader, lastMod.Revision())
}

// Shallow reports whether the repository is a shallow clone. Git cuts the history
// of a shallow clone at its boundary commits, so the walk of this package, which
// follows the parents stored in the objects, would not match git log. Callers must
// use Git for shallow repositories.
func (d *Deriver) Shallow(ctx context.Context) (bool, error) {
	repoPath, err := d.repo.Path(ctx)
	if err != nil {
		return false, fmt.Errorf("getting repository path: %w", err)
	}

	_, err = os.Stat(filepath.Join(repoPath, "shallow"))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("checking for shallow file: %w", err)
	}
}

// ResolveCommit returns the commit ID of revision. It returns
// catfile.NotFoundError when the revision does not name a commit.
func (d *Deriver) ResolveCommit(ctx context.Context, revision git.Revision) (git.ObjectID, error) {
	return d.resolveCommit(ctx, revision)
}

func (d *Deriver) resolveCommit(ctx context.Context, revision git.Revision) (git.ObjectID, error) {
	commitID, err := d.repo.ResolveRevision(ctx, revision+"^{commit}")
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return "", catfile.NotFoundError{Revision: revision.String()}
		}
		return "", fmt.Errorf("resolving revision %q: %w", revision, err)
	}

	return commitID, nil
}

// TreeEntry returns the tree entry at path in commit, or nil when path does not
// exist there. The root directory is returned as a tree entry with the path ".".
func (d *Deriver) TreeEntry(ctx context.Context, commit git.ObjectID, path string) (*gitalypb.TreeEntry, error) {
	path = normalizePath(path)
	if path == "." {
		commitObject, err := catfile.GetCommit(ctx, d.reader, commit.Revision())
		if err != nil {
			return nil, fmt.Errorf("reading commit %s: %w", commit, err)
		}
		return &gitalypb.TreeEntry{
			Oid:  commitObject.GetTreeId(),
			Mode: treeMode,
			Type: gitalypb.TreeEntry_TREE,
			Path: []byte("."),
		}, nil
	}

	entries, err := catfile.TreeEntries(ctx, d.reader, commit.String(), pathpkg.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("reading tree entries of %s: %w", commit, err)
	}

	for _, entry := range entries {
		if string(entry.GetPath()) == path {
			return entry, nil
		}
	}

	return nil, nil
}
