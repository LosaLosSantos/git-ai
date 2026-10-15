package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrNotWorkspace is returned when a directory has no .graft/state.json.
	ErrNotWorkspace = errors.New("not a graft workspace (no .graft/state.json found)")
	// ErrNothingToCommit is returned by Commit when no tracked or new file changed.
	ErrNothingToCommit = errors.New("nothing to commit")
)

// maxBatchOIDs bounds the number of object IDs per Backend.GetObjects call, so
// that the contents of one batch are verified and written before the next is
// fetched. It is not the server limit: the sdk-backed backend splits a batch
// into requests as small as the server's max_objects requires.
const maxBatchOIDs = 500

// ConflictError lists local paths that block an update or a sparse change.
// Nothing is written when a conflict is reported.
type ConflictError struct {
	Paths []string
}

func (e *ConflictError) Error() string {
	return "local changes would be overwritten: " + strings.Join(e.Paths, ", ")
}

// ChangeKind is the status of a path relative to the base commit.
type ChangeKind string

// Change kinds reported by Status.
const (
	ChangeAdded    ChangeKind = "A"
	ChangeModified ChangeKind = "M"
	ChangeDeleted  ChangeKind = "D"
)

// Change is one entry of the local status.
type Change struct {
	Path string
	Kind ChangeKind
}

// CloneOptions configure Clone.
type CloneOptions struct {
	Address string
	Storage string
	Repo    string
	Branch  string
	Sparse  []string
}

// CommitRequest describes a commit of the local changes.
type CommitRequest struct {
	Message string
	Author  Signature
	// User names the acting user in the temporary branch
	// graft/<user>/<random>, so that access policies can grant each agent its
	// own temporary branches. The author name is used when it is empty. A name
	// that is not a valid reference name component is turned into a lowercase
	// slug (see temporaryBranch).
	User       string
	LandBranch string
	Strategy   string
	// Resolve asks the server to resolve merge conflicts when landing.
	Resolve bool
}

// CommitResult reports the commit created by Commit.
type CommitResult struct {
	Commit  string
	Branch  string
	Landed  bool
	Changes int
	// ResolvedPaths are the files the server resolved while landing.
	ResolvedPaths []string
	// BatchSize, BatchPosition and Attempts are those of the landing, as in
	// LandResult. They are zero when the commit was not landed.
	BatchSize     int32
	BatchPosition int32
	Attempts      int32
}

// UpdateResult reports the base commit before and after an update.
type UpdateResult struct {
	Old string
	New string
}

// Workspace is a sparse working copy rooted at a directory.
type Workspace struct {
	root    string
	backend Backend
	state   State
	// skipped lists the paths that the last sync did not materialize, with the
	// reason for each. See Skipped.
	skipped []string
}

// Find returns the workspace root that contains start.
func Find(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(statePath(dir)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotWorkspace
		}
		dir = parent
	}
}

// Open loads the workspace at root and binds it to backend.
func Open(root string, backend Backend) (*Workspace, error) {
	if backend == nil {
		return nil, errors.New("workspace: nil backend")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	st, err := loadState(root)
	if err != nil {
		return nil, err
	}
	return &Workspace{root: root, backend: backend, state: st}, nil
}

// lockAndLoad takes the workspace lock and then reloads the state from disk.
// Open reads the state before any lock is held, so a command that waited for
// another one must not work on, and later save, the state it read earlier: that
// would erase the changes the other command saved meanwhile, such as a pending
// commit. The returned function releases the lock.
func (w *Workspace) lockAndLoad() (func(), error) {
	unlock, err := lock(w.root)
	if err != nil {
		return nil, err
	}
	st, err := loadState(w.root)
	if err != nil {
		unlock()
		return nil, err
	}
	w.state = st
	return unlock, nil
}

// tempDir returns the directory for the temporary files of atomic writes into
// the workspace. It lies in the metadata directory, so that a temporary file
// never shows up as a local change, whatever the repository's file names are.
func (w *Workspace) tempDir() string {
	return filepath.Join(w.root, metaDirName, "tmp")
}

// Clone creates a workspace in root (which must be empty or absent) and
// materializes the sparse part of branch.
func Clone(ctx context.Context, root string, backend Backend, opts CloneOptions) (*Workspace, error) {
	if backend == nil {
		return nil, errors.New("workspace: nil backend")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	branch := opts.Branch
	if branch == "" {
		branch = "main"
	}
	sparse, err := normalizeSparse(opts.Sparse)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	switch {
	case err == nil && len(entries) > 0:
		return nil, fmt.Errorf("clone directory %q is not empty", root)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	existed := err == nil
	base, err := backend.ResolveRevision(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("resolve branch %q: %w", branch, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	w := &Workspace{
		root:    root,
		backend: backend,
		state: State{
			Version:      stateVersion,
			Address:      opts.Address,
			Storage:      opts.Storage,
			Repo:         opts.Repo,
			Branch:       branch,
			ObjectFormat: formatOf(base),
			Sparse:       sparse,
			Files:        map[string]FileState{},
		},
	}
	if err := w.sync(ctx, base, sparse); err != nil {
		// A clone that did not finish, for example because it was interrupted
		// while fetching objects, would leave a workspace without a base commit.
		// It is removed, so that the clone can simply be run again.
		if cleanupErr := w.removeIncompleteClone(existed); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove the incomplete clone in %s: %w", root, cleanupErr))
		}
		return nil, err
	}
	return w, nil
}

// removeIncompleteClone removes what a failed Clone wrote: the tracked files,
// the metadata directory (which also holds leftover temporary files) and the
// directories that end up empty. The clone directory itself is removed when
// Clone created it. Files that Clone did not write are kept.
func (w *Workspace) removeIncompleteClone(existed bool) error {
	var errs []error
	for p := range w.state.Files {
		full, err := localPath(w.root, p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	errs = append(errs, os.RemoveAll(filepath.Join(w.root, metaDirName)))
	entries, err := os.ReadDir(w.root)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, e := range entries {
		if e.IsDir() {
			removeEmptyTree(filepath.Join(w.root, e.Name()))
		}
	}
	if !existed {
		// The directory is kept if it still holds files that Clone did not write.
		_ = os.Remove(w.root)
	}
	return errors.Join(errs...)
}

// requireBase refuses to work on a workspace whose clone did not finish. Such a
// workspace has no base commit, so every file would look new.
func (w *Workspace) requireBase() error {
	if w.state.Base == "" {
		return errors.New("the workspace has no base commit because its clone did not finish: run graft update to finish it")
	}
	return nil
}

// Root returns the workspace directory.
func (w *Workspace) Root() string { return w.root }

// Skipped describes the repository paths that the last clone, update, sparse
// change or refresh after a land did not materialize, because they are reserved
// (such as a nested ".graft" directory) or are not safe file names on this
// system. Each entry names the path and the reason. Such paths are neither
// tracked nor changed by commits, so they stay as they are on the server.
func (w *Workspace) Skipped() []string {
	return append([]string(nil), w.skipped...)
}

// Summary describes the workspace state.
func (w *Workspace) Summary() Summary {
	return Summary{
		Base:    w.state.Base,
		Branch:  w.state.Branch,
		Files:   len(w.state.Files),
		Sparse:  append([]string(nil), w.state.Sparse...),
		Pending: w.state.Pending,
	}
}

// Status compares the workspace with its base commit. It skips re-hashing a
// file whose size and mtime are unchanged. Files that are not tracked are
// reported as added, including files outside the sparse set that exist on the
// server; Commit reports those.
//
// A file that was re-hashed and found unchanged is recorded with its current
// stat values once its mtime is old enough to be trusted, so that later calls
// skip re-hashing it. This rewrites the state file only when there is something
// to record.
func (w *Workspace) Status() ([]Change, error) {
	unlock, err := w.lockAndLoad()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return w.status()
}

// status is Status without taking the workspace lock.
func (w *Workspace) status() ([]Change, error) {
	if err := w.requireBase(); err != nil {
		return nil, err
	}
	kinds, verified, err := w.scan()
	if err != nil {
		return nil, err
	}
	if len(verified) > 0 {
		for p, st := range verified {
			w.state.Files[p] = st
		}
		if err := w.state.save(w.root); err != nil {
			return nil, err
		}
	}
	out := make([]Change, 0, len(kinds))
	for p, k := range kinds {
		out = append(out, Change{Path: p, Kind: k})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Commit writes the local changes on a temporary branch on top of the base
// commit. With LandBranch set it then lands the commit. A failed land leaves
// the commit pending, so Land can be retried.
func (w *Workspace) Commit(ctx context.Context, req CommitRequest) (CommitResult, error) {
	if strings.TrimSpace(req.Message) == "" {
		return CommitResult{}, errors.New("commit message is required")
	}
	if req.LandBranch != "" {
		if err := checkStrategy(req.Strategy); err != nil {
			return CommitResult{}, err
		}
	}
	unlock, err := w.lockAndLoad()
	if err != nil {
		return CommitResult{}, err
	}
	defer unlock()

	if w.state.Pending != nil {
		return CommitResult{}, w.pendingError("committing")
	}
	changes, err := w.status()
	if err != nil {
		return CommitResult{}, err
	}
	if len(changes) == 0 {
		return CommitResult{}, ErrNothingToCommit
	}

	// Deletions come first: a file that replaces a directory can only be
	// created once the files in that directory are gone, and status order puts
	// "dir" before "dir/f.txt".
	var actions []Action
	deleted := map[string]bool{}
	for _, c := range changes {
		if c.Kind == ChangeDeleted {
			deleted[c.Path] = true
			actions = append(actions, Action{Kind: ActionDelete, Path: c.Path})
		}
	}
	committed := map[string]FileState{}
	blobs := map[string][]byte{}
	pending := Pending{Before: map[string]FileState{}}
	for _, c := range changes {
		if prior, tracked := w.state.Files[c.Path]; tracked {
			pending.Before[c.Path] = prior
		} else {
			pending.Added = append(pending.Added, c.Path)
		}
		if c.Kind == ChangeDeleted {
			continue
		}
		if c.Kind == ChangeAdded {
			if err := w.checkNotOnServer(ctx, c.Path, deleted); err != nil {
				return CommitResult{}, err
			}
		}
		full, err := localPath(w.root, c.Path)
		if err != nil {
			return CommitResult{}, err
		}
		info, err := os.Lstat(full)
		if err != nil {
			return CommitResult{}, err
		}
		if !info.Mode().IsRegular() {
			return CommitResult{}, fmt.Errorf("%s is not a regular file (symbolic links are not supported yet)", c.Path)
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return CommitResult{}, err
		}
		mode := w.diskMode(c.Path, info)
		oid := BlobID(content, w.state.sha256Format())
		prior, tracked := w.state.Files[c.Path]
		switch {
		case !tracked:
			actions = append(actions, Action{Kind: ActionPut, Path: c.Path, Content: content, Executable: mode == modeExec})
		default:
			// The server keeps the mode of an overwritten file, so a changed mode
			// needs its own action.
			if oid != prior.OID {
				actions = append(actions, Action{Kind: ActionPut, Path: c.Path, Content: content, Overwrite: true})
			}
			if mode != prior.Mode {
				actions = append(actions, Action{Kind: ActionChmod, Path: c.Path, Executable: mode == modeExec})
			}
		}
		blobs[oid] = content
		committed[c.Path] = FileState{
			OID:     oid,
			Mode:    mode,
			Size:    info.Size(),
			MTimeNS: info.ModTime().UnixNano(),
		}
	}

	// The committed content is cached like any fetched object, so that diff and
	// the refresh after a land can read it without contacting the server.
	for oid, content := range blobs {
		if err := writeCachedObject(w.root, oid, content); err != nil {
			return CommitResult{}, err
		}
	}

	owner := req.User
	if owner == "" {
		owner = req.Author.Name
	}
	branch, err := temporaryBranch(owner)
	if err != nil {
		return CommitResult{}, err
	}
	commit, err := w.backend.CommitFiles(ctx, CommitOptions{
		Branch:   branch,
		StartRev: w.state.Base,
		Message:  req.Message,
		Author:   req.Author,
		Actions:  actions,
	})
	if err != nil {
		return CommitResult{}, fmt.Errorf("commit files: %w", err)
	}

	for _, c := range changes {
		if c.Kind == ChangeDeleted {
			delete(w.state.Files, c.Path)
		}
	}
	for p, st := range committed {
		w.state.Files[p] = st
	}
	pending.Branch = branch
	pending.Commit = commit
	w.state.Pending = &pending
	if err := w.state.save(w.root); err != nil {
		return CommitResult{}, err
	}

	res := CommitResult{Commit: commit, Branch: branch, Changes: len(actions)}
	if req.LandBranch == "" {
		return res, nil
	}
	lres, err := w.land(ctx, req.LandBranch, req.Strategy, req.Author, req.Resolve)
	if err != nil {
		if w.state.Pending != nil && w.state.Pending.Landed != "" {
			// The land succeeded; only the refresh failed, and err explains it.
			return res, fmt.Errorf("committed %s: %w", commit, err)
		}
		return res, fmt.Errorf("committed %s on %s but landing failed (run graft land to retry, or graft land --abort to drop it): %w", commit, branch, err)
	}
	res.Landed = true
	res.ResolvedPaths = lres.ResolvedPaths
	res.BatchSize = lres.BatchSize
	res.BatchPosition = lres.BatchPosition
	res.Attempts = lres.Attempts
	return res, nil
}

// Land lands the pending commit onto branch (the workspace branch when empty),
// recording committer as the committer of rewritten commits. On success the
// pending commit is cleared, the base moves to the landed commit and the
// workspace is refreshed. A failed land keeps the commit pending.
func (w *Workspace) Land(ctx context.Context, branch, strategy string, committer Signature) (LandResult, error) {
	return w.LandResolving(ctx, branch, strategy, committer, false)
}

// LandResolving is Land with the option to have the server resolve merge
// conflicts with its conflict resolver (resolve).
func (w *Workspace) LandResolving(ctx context.Context, branch, strategy string, committer Signature, resolve bool) (LandResult, error) {
	unlock, err := w.lockAndLoad()
	if err != nil {
		return LandResult{}, err
	}
	defer unlock()
	return w.land(ctx, branch, strategy, committer, resolve)
}

// land is Land without taking the workspace lock.
func (w *Workspace) land(ctx context.Context, branch, strategy string, committer Signature, resolve bool) (LandResult, error) {
	if w.state.Pending == nil {
		return LandResult{}, errors.New("nothing to land: no pending commit")
	}
	if branch == "" {
		branch = w.state.Branch
	}
	if strategy == "" {
		strategy = StrategyRebase
	}
	if err := checkStrategy(strategy); err != nil {
		return LandResult{}, err
	}
	pending := w.state.Pending
	var res LandResult
	if pending.Landed != "" {
		// The server already accepted the land; only the refresh is left. Landing
		// the original commit again could rebase it a second time.
		if branch != w.state.Branch {
			return LandResult{}, fmt.Errorf("commit %s already landed on %s as %s: run graft land without a branch to refresh the workspace", pending.Commit, w.state.Branch, pending.Landed)
		}
		res = LandResult{Commit: pending.Landed, AlreadyLanded: true}
	} else {
		var err error
		res, err = w.backend.Land(ctx, LandOptions{
			Branch:   branch,
			Commit:   pending.Commit,
			Strategy: strategy,
			// Only the workspace's own branch may be created here. A missing target
			// branch is an error, so a mistyped name does not create a new branch.
			CreateBranch:     branch == w.state.Branch,
			Committer:        committer,
			ResolveConflicts: resolve,
		})
		if err != nil {
			return LandResult{}, err
		}

		// The base follows the landed commit, so the workspace now tracks the
		// branch it landed on. Otherwise the next default land would carry that
		// branch's history into the workspace branch.
		w.state.Branch = branch
		// The land is recorded before the refresh, so that a failed refresh
		// cannot lead to an abort of a commit that is already on the branch.
		pending.Landed = res.Commit
		if err := w.state.save(w.root); err != nil {
			return res, fmt.Errorf("landed as %s but saving the workspace state failed: %w", res.Commit, err)
		}
	}
	// The temporary branch is no longer needed. A failure to delete it is harmless.
	_ = w.backend.DeleteBranch(ctx, pending.Branch)

	// The pending commit stays recorded until the workspace shows the landed
	// tree. If the refresh fails, commits are refused, because they would start
	// from a stale base. A later Land retries the refresh, and Sparse refreshes
	// with another sparse set.
	if err := w.sync(ctx, res.Commit, w.state.Sparse); err != nil {
		return res, refreshError(res.Commit, err)
	}
	if err := w.finishLand(); err != nil {
		return res, fmt.Errorf("landed as %s but saving the workspace state failed: %w", res.Commit, err)
	}
	return res, nil
}

// finishLand clears a landed pending commit once the workspace shows the landed
// tree.
func (w *Workspace) finishLand() error {
	w.state.Pending = nil
	return w.state.save(w.root)
}

// refreshError explains a failed refresh of the workspace after a land, with the
// way out that applies to the failure.
func refreshError(landed string, err error) error {
	var conflict *ConflictError
	if errors.As(err, &conflict) {
		return fmt.Errorf("landed as %s but refreshing the workspace failed (revert or restore the conflicting local changes, then run graft land again): %w", landed, err)
	}
	return fmt.Errorf("landed as %s but refreshing the workspace failed (run graft land again to retry, or graft sparse to refresh it with a sparse set that leaves out the failing paths): %w", landed, err)
}

// checkNotOnServer fails when a locally added path already exists at the base
// commit. Such a file is outside the sparse set, because every tracked path of
// the sparse set was materialized at the base.
//
// A directory at the base may be replaced by the added file only when the
// commit deletes every file in it, which are the paths in deleted. A file that
// the commit does not delete is outside the sparse set and keeps the directory.
func (w *Workspace) checkNotOnServer(ctx context.Context, p string, deleted map[string]bool) error {
	_, err := w.backend.ReadFile(ctx, w.state.Base, p)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err == nil:
		return fmt.Errorf("%w: %s exists on the server outside the sparse set (run graft sparse to materialize it, or rename the local file)", ErrAlreadyExists, p)
	}

	// p is not a file at the base. Reading it fails for a directory too, so
	// check whether it lists as one.
	entries, listErr := w.backend.ListTree(ctx, w.state.Base, p, true)
	if listErr != nil {
		return fmt.Errorf("check %q at %s: %w", p, shortRev(w.state.Base), err)
	}
	var kept []string
	for _, e := range entries {
		if e.Type == ObjectTree {
			continue
		}
		if full := joinRel(p, e.Path); !deleted[full] {
			kept = append(kept, full)
		}
	}
	if len(kept) > 0 {
		sort.Strings(kept)
		const shown = 5
		list := strings.Join(kept[:min(len(kept), shown)], ", ")
		if len(kept) > shown {
			list += fmt.Sprintf(" and %d more", len(kept)-shown)
		}
		return fmt.Errorf("%w: %s is a directory on the server that holds files outside the sparse set (%s): run graft sparse to materialize them and remove them, or rename the local file", ErrAlreadyExists, p, list)
	}
	return nil
}

// shortRev abbreviates a commit ID for messages.
func shortRev(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

// Update moves the base to the current tip of the workspace branch. It refuses,
// without changing anything, when local changes touch files that would change.
func (w *Workspace) Update(ctx context.Context) (UpdateResult, error) {
	unlock, err := w.lockAndLoad()
	if err != nil {
		return UpdateResult{}, err
	}
	defer unlock()

	if w.state.Pending != nil {
		return UpdateResult{}, w.pendingError("updating")
	}
	tip, err := w.backend.ResolveRevision(ctx, w.state.Branch)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("resolve branch %q: %w", w.state.Branch, err)
	}
	old := w.state.Base
	if err := w.sync(ctx, tip, w.state.Sparse); err != nil {
		return UpdateResult{}, err
	}
	return UpdateResult{Old: old, New: tip}, nil
}

// Sparse changes the sparse set to prefixes (the whole repository when empty)
// and materializes or removes files accordingly.
//
// When the pending commit has landed and only the refresh of the workspace is
// left, Sparse finishes that refresh with the new sparse set: the workspace
// moves to the landed commit and the commit is no longer pending. A refresh that
// keeps failing on some paths can so always be finished, by leaving them out.
func (w *Workspace) Sparse(ctx context.Context, prefixes []string) error {
	unlock, err := w.lockAndLoad()
	if err != nil {
		return err
	}
	defer unlock()

	pending := w.state.Pending
	if pending != nil && pending.Landed == "" {
		return w.pendingError("changing the sparse set")
	}
	if err := w.requireBase(); err != nil {
		return err
	}
	sparse, err := normalizeSparse(prefixes)
	if err != nil {
		return err
	}
	if pending == nil {
		return w.sync(ctx, w.state.Base, sparse)
	}
	if err := w.sync(ctx, pending.Landed, sparse); err != nil {
		return refreshError(pending.Landed, err)
	}
	return w.finishLand()
}

// Abort drops the pending commit. It deletes the temporary branch and restores
// the tracking state of the files the commit changed, so the local edits show
// again as changes against the base. The local files are not touched. Use it
// after a refused land, then Revert the paths to take the server's version, or
// keep them and commit again after Update.
//
// Abort refuses once the commit has landed and only the workspace refresh
// failed: the commit is on the branch, so dropping it would show its changes as
// local edits again. Revert the conflicting local changes and Land again to
// finish the refresh instead, or finish it with Sparse.
func (w *Workspace) Abort(ctx context.Context) error {
	unlock, err := w.lockAndLoad()
	if err != nil {
		return err
	}
	defer unlock()

	if w.state.Pending == nil {
		return errors.New("nothing to abort: no pending commit")
	}
	pending := *w.state.Pending
	if pending.Landed != "" {
		return fmt.Errorf("commit %s already landed on %s as %s and cannot be dropped: run graft land to refresh the workspace (revert or restore conflicting local changes first), or graft sparse to refresh it with a sparse set that leaves out the failing paths", pending.Commit, w.state.Branch, pending.Landed)
	}
	// A failure to delete the temporary branch leaves an unused branch behind.
	_ = w.backend.DeleteBranch(ctx, pending.Branch)
	for p, st := range pending.Before {
		w.state.Files[p] = st
	}
	for _, p := range pending.Added {
		delete(w.state.Files, p)
	}
	w.state.Pending = nil
	return w.state.save(w.root)
}

// Revert discards local changes to paths and restores them to the base commit.
// Each path names a file or a directory; a directory selects every changed path
// under it. Changed paths are reverted when no path is given. A tracked file
// gets its base content and mode, and an untracked file is removed. A path that
// matches neither a changed nor a tracked file is an error, and then nothing is
// reverted.
//
// Revert refuses while a commit is pending, so that no local edit is lost before
// it is landed or dropped. Once the pending commit has landed and only the
// workspace refresh is left, the commit is safe on the server, and Revert of
// paths that the commit did not change resolves the conflicts that block the
// refresh.
func (w *Workspace) Revert(ctx context.Context, paths []string) error {
	unlock, err := w.lockAndLoad()
	if err != nil {
		return err
	}
	defer unlock()

	if pending := w.state.Pending; pending != nil && pending.Landed == "" {
		return w.pendingError("reverting")
	}
	selected := make([]string, 0, len(paths))
	for _, p := range paths {
		clean, err := ValidatePath(p)
		if err != nil {
			return fmt.Errorf("revert: %w", err)
		}
		selected = append(selected, clean)
	}
	changes, err := w.status()
	if err != nil {
		return err
	}

	var targets []string
	matched := make([]bool, len(selected))
	for _, c := range changes {
		hit := len(selected) == 0
		for i, s := range selected {
			if underPath(c.Path, s) {
				matched[i] = true
				hit = true
			}
		}
		if hit {
			targets = append(targets, c.Path)
		}
	}
	for i, s := range selected {
		if !matched[i] && !w.tracksUnder(s) {
			return fmt.Errorf("revert: %s does not match any file", s)
		}
	}
	if pending := w.state.Pending; pending != nil {
		for _, p := range targets {
			if w.pendingChanged(p) {
				return fmt.Errorf("revert: %s is part of the landed commit %s; run graft land to refresh the workspace first", p, pending.Landed)
			}
		}
	}

	for _, p := range targets {
		if err := w.revertPath(ctx, p); err != nil {
			return err
		}
	}
	return w.state.save(w.root)
}

// pendingError refuses an action while a commit is pending, with the way out
// that applies to the pending commit.
func (w *Workspace) pendingError(action string) error {
	p := w.state.Pending
	if p.Landed != "" {
		return fmt.Errorf("commit %s is pending: it landed as %s but the workspace was not refreshed: run graft land to refresh it (revert or restore conflicting local changes first), or graft sparse to refresh it with a sparse set that leaves out the failing paths, before %s", shortRev(p.Commit), shortRev(p.Landed), action)
	}
	return fmt.Errorf("commit %s on %s is pending: land it, or drop it with graft land --abort, before %s", shortRev(p.Commit), p.Branch, action)
}

// underPath reports whether p is dir itself or lies under it.
func underPath(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// tracksUnder reports whether a tracked file is p or lies under p.
func (w *Workspace) tracksUnder(p string) bool {
	for tracked := range w.state.Files {
		if underPath(tracked, p) {
			return true
		}
	}
	return false
}

// pendingChanged reports whether the pending commit changed p.
func (w *Workspace) pendingChanged(p string) bool {
	pending := w.state.Pending
	if pending == nil {
		return false
	}
	if _, ok := pending.Before[p]; ok {
		return true
	}
	for _, added := range pending.Added {
		if added == p {
			return true
		}
	}
	return false
}

// revertPath restores one changed path to the base commit. It does not save the
// state.
func (w *Workspace) revertPath(ctx context.Context, p string) error {
	full, err := localPath(w.root, p)
	if err != nil {
		return err
	}
	desired, inBase, err := w.fileAt(ctx, w.state.Base, p)
	if err != nil {
		return err
	}
	if !inBase {
		if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		w.removeEmptyDirs(filepath.Dir(full))
		delete(w.state.Files, p)
		return nil
	}
	if err := w.fetch(ctx, []string{desired.OID}); err != nil {
		return err
	}
	content, err := w.object(desired.OID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	perm := fs.FileMode(0o644)
	if desired.Mode == modeExec {
		perm = 0o755
	}
	if err := writeFileAtomic(w.tempDir(), full, content, perm); err != nil {
		return err
	}
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	w.state.Files[p] = FileState{OID: desired.OID, Mode: desired.Mode, Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}
	return nil
}

// scan compares the disk with the tracked files. It returns the changed paths
// and, for tracked files that were re-hashed and found unchanged, the stat
// values to record so that they are trusted from now on.
func (w *Workspace) scan() (map[string]ChangeKind, map[string]FileState, error) {
	now := time.Now().UnixNano()
	present := map[string]fs.FileInfo{}
	links := map[string]bool{}
	err := filepath.WalkDir(w.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == w.root {
			return nil
		}
		rel, err := filepath.Rel(w.root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if isReservedPath(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// A symbolic link is reported, never followed, so it is neither silently
		// ignored nor read through. Other special files are skipped.
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			links[rel] = true
		case !info.Mode().IsRegular():
			return nil
		}
		present[rel] = info
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	kinds := map[string]ChangeKind{}
	verified := map[string]FileState{}
	for p, tracked := range w.state.Files {
		info, ok := present[p]
		if !ok {
			kinds[p] = ChangeDeleted
			continue
		}
		if links[p] {
			kinds[p] = ChangeModified
			continue
		}
		if w.trusted(p, tracked, info) {
			continue
		}
		full, err := localPath(w.root, p)
		if err != nil {
			return nil, nil, err
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return nil, nil, err
		}
		mode := w.diskMode(p, info)
		if BlobID(content, w.state.sha256Format()) != tracked.OID || mode != tracked.Mode {
			kinds[p] = ChangeModified
			continue
		}
		mtime := info.ModTime().UnixNano()
		if tracked.Size != info.Size() || tracked.MTimeNS != mtime || mtime < now-int64(racyWindow) {
			verified[p] = FileState{OID: tracked.OID, Mode: tracked.Mode, Size: info.Size(), MTimeNS: mtime}
		}
	}
	for p := range present {
		if _, tracked := w.state.Files[p]; !tracked {
			kinds[p] = ChangeAdded
		}
	}
	return kinds, verified, nil
}

// trusted reports whether a tracked file can be assumed unchanged from its size,
// mtime and mode alone. A file whose mtime is not clearly older than the last
// state save is racy: it may have changed within the timestamp granularity
// without its size or mtime showing it, so it is re-hashed instead.
func (w *Workspace) trusted(p string, tracked FileState, info fs.FileInfo) bool {
	mtime := info.ModTime().UnixNano()
	if tracked.Size != info.Size() || tracked.MTimeNS != mtime || tracked.Mode != w.diskMode(p, info) {
		return false
	}
	return mtime < w.state.SavedNS-int64(racyWindow)
}

// diskMode returns the Git mode of a file on disk. On Windows the executable
// bit is not meaningful, so the recorded mode is kept.
func (w *Workspace) diskMode(p string, info fs.FileInfo) uint32 {
	if runtime.GOOS == "windows" {
		if tracked, ok := w.state.Files[p]; ok {
			return tracked.Mode
		}
		return modeRegular
	}
	if info.Mode().Perm()&0o111 != 0 {
		return modeExec
	}
	return modeRegular
}

// sync makes the workspace show base restricted to sparse. Local changes are
// kept; if a file the sync must write or remove has local changes, sync returns
// a ConflictError and writes nothing.
func (w *Workspace) sync(ctx context.Context, base string, sparse []string) (err error) {
	desired, err := w.desiredTree(ctx, base, sparse)
	if err != nil {
		return err
	}
	changes, _, err := w.scan()
	if err != nil {
		return err
	}

	var conflicts, write, remove []string
	for p, d := range desired {
		old, tracked := w.state.Files[p]
		if tracked && old.OID == d.OID && old.Mode == d.Mode {
			continue
		}
		if _, dirty := changes[p]; dirty {
			conflicts = append(conflicts, p)
			continue
		}
		write = append(write, p)
	}
	var forget []string
	for p := range w.state.Files {
		if _, ok := desired[p]; ok {
			continue
		}
		switch changes[p] {
		case ChangeModified, ChangeAdded:
			conflicts = append(conflicts, p)
		case ChangeDeleted:
			forget = append(forget, p)
		default:
			remove = append(remove, p)
		}
	}
	removing := make(map[string]bool, len(remove))
	for _, p := range remove {
		removing[p] = true
	}
	conflicts = append(conflicts, w.blockedWrites(write, removing)...)
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return &ConflictError{Paths: conflicts}
	}
	sort.Strings(write)
	sort.Strings(remove)

	// Files that were written or removed before a failure must stay accounted
	// for, so the tracked state is saved even though the base does not move.
	defer func() {
		if err != nil {
			if saveErr := w.state.save(w.root); saveErr != nil {
				err = errors.Join(err, saveErr)
			}
		}
	}()

	for _, p := range append(append([]string(nil), write...), remove...) {
		if _, err := localPath(w.root, p); err != nil {
			return err
		}
	}

	oids := make([]string, 0, len(write))
	for _, p := range write {
		oids = append(oids, desired[p].OID)
	}
	if err := w.fetch(ctx, oids); err != nil {
		return err
	}

	for _, p := range remove {
		full, err := localPath(w.root, p)
		if err != nil {
			return err
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		w.removeEmptyDirs(filepath.Dir(full))
		delete(w.state.Files, p)
	}
	for _, p := range forget {
		delete(w.state.Files, p)
	}
	for _, p := range write {
		d := desired[p]
		content, err := w.object(d.OID)
		if err != nil {
			return err
		}
		full, err := localPath(w.root, p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		// Empty directories left where the file goes are not local changes, so
		// they are removed. blockedWrites already refused any content.
		if info, err := os.Lstat(full); err == nil && info.IsDir() {
			removeEmptyTree(full)
		}
		perm := fs.FileMode(0o644)
		if d.Mode == modeExec {
			perm = 0o755
		}
		if err := writeFileAtomic(w.tempDir(), full, content, perm); err != nil {
			return err
		}
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		w.state.Files[p] = FileState{OID: d.OID, Mode: d.Mode, Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}
	}

	w.state.Base = base
	w.state.Sparse = sparse
	return w.state.save(w.root)
}

type desiredFile struct {
	OID  string
	Mode uint32
}

// desiredTree lists the blobs of base that fall inside the sparse set.
// Prefixes that do not exist at base select nothing.
func (w *Workspace) desiredTree(ctx context.Context, base string, sparse []string) (map[string]desiredFile, error) {
	prefixes := sparse
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}
	w.skipped = nil
	out := map[string]desiredFile{}
	for _, prefix := range prefixes {
		entries, err := w.backend.ListTree(ctx, base, prefix, true)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			// A prefix that names a file selects that file alone.
			file, isFile, fileErr := w.fileAt(ctx, base, prefix)
			if fileErr != nil || !isFile {
				return nil, fmt.Errorf("list %q at %s: %w", prefix, base, err)
			}
			out[prefix] = file
			continue
		}
		for _, e := range entries {
			if e.Type != ObjectBlob {
				continue
			}
			p := joinRel(prefix, e.Path)
			// A path that cannot be written safely here is left out instead of
			// failing the sync: one such path must not make the whole repository,
			// or the refresh after a land, impossible to materialize.
			if _, err := ValidatePath(p); err != nil {
				w.skipped = append(w.skipped, err.Error())
				continue
			}
			if !validOID(e.OID) {
				return nil, fmt.Errorf("server returned an invalid object ID for %s", p)
			}
			// TODO(graft-complete): materialize symbolic links instead of writing
			// their target as a regular file.
			out[p] = desiredFile{OID: e.OID, Mode: normalizeMode(e.Mode)}
		}
	}
	return out, nil
}

func normalizeMode(mode uint32) uint32 {
	switch {
	case mode == modeExec:
		return modeExec
	default:
		return modeRegular
	}
}

// fetch makes sure every object in oids is in the local cache. Missing objects
// are fetched in batches and verified against their object ID before storing.
func (w *Workspace) fetch(ctx context.Context, oids []string) error {
	seen := map[string]bool{}
	var missing []string
	for _, oid := range oids {
		if seen[oid] {
			continue
		}
		seen[oid] = true
		_, ok, err := readCachedObject(w.root, oid)
		if err != nil {
			return err
		}
		if !ok {
			missing = append(missing, oid)
		}
	}
	for len(missing) > 0 {
		batch := missing[:min(len(missing), maxBatchOIDs)]
		missing = missing[len(batch):]
		got, err := w.backend.GetObjects(ctx, batch)
		if err != nil {
			return fmt.Errorf("fetch objects: %w", err)
		}
		for _, oid := range batch {
			content, ok := got[oid]
			if !ok {
				return fmt.Errorf("object %s not found on the server", oid)
			}
			if BlobID(content, w.state.sha256Format()) != oid {
				return fmt.Errorf("object %s does not match its object ID", oid)
			}
			if err := writeCachedObject(w.root, oid, content); err != nil {
				return err
			}
		}
	}
	return nil
}

// object returns a blob from the local cache, fetching it when needed.
func (w *Workspace) object(oid string) ([]byte, error) {
	content, ok, err := readCachedObject(w.root, oid)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("object %s is not in the local cache", oid)
	}
	return content, nil
}

// removeEmptyDirs removes dir and its empty parents, stopping at the root. The
// root is absolute (see Open and Clone), so the prefix check is exact.
func (w *Workspace) removeEmptyDirs(dir string) {
	for dir != w.root && strings.HasPrefix(dir, w.root+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// removeEmptyTree removes the empty directories under dir, and dir itself when it
// ends up empty. Directories that hold files are kept.
func removeEmptyTree(dir string) {
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
}

// blockedWrites returns the local paths that stand in the way of writing the
// paths in write. Removed paths are not blocking. An untracked or changed file
// is blocking, so it is never overwritten or hidden by a directory. A regular
// file that is an ancestor of a written path blocks it. A directory at a written
// path blocks it when it holds files that stay.
func (w *Workspace) blockedWrites(write []string, removing map[string]bool) []string {
	var blocked []string
	for _, p := range write {
		parts := strings.Split(p, "/")
		for i := 1; i < len(parts); i++ {
			ancestor := strings.Join(parts[:i], "/")
			info, err := os.Lstat(filepath.Join(w.root, filepath.FromSlash(ancestor)))
			if err == nil && info.Mode().IsRegular() && !removing[ancestor] {
				blocked = append(blocked, ancestor)
			}
		}

		info, err := os.Lstat(filepath.Join(w.root, filepath.FromSlash(p)))
		if err != nil || !info.IsDir() {
			continue
		}
		_ = filepath.WalkDir(filepath.Join(w.root, filepath.FromSlash(p)), func(sub string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(w.root, sub)
			if err != nil {
				return nil
			}
			if rel = filepath.ToSlash(rel); !removing[rel] {
				blocked = append(blocked, rel)
			}
			return nil
		})
	}
	return blocked
}

// fileAt returns the blob at p in rev. The bool is false when p is not a file at
// rev.
func (w *Workspace) fileAt(ctx context.Context, rev, p string) (desiredFile, bool, error) {
	dir, name := "", p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		dir, name = p[:i], p[i+1:]
	}
	entries, err := w.backend.ListTree(ctx, rev, dir, false)
	if errors.Is(err, ErrNotFound) {
		return desiredFile{}, false, nil
	}
	if err != nil {
		return desiredFile{}, false, err
	}
	for _, e := range entries {
		if e.Path != name || e.Type != ObjectBlob {
			continue
		}
		if !validOID(e.OID) {
			return desiredFile{}, false, fmt.Errorf("server returned an invalid object ID for %s", p)
		}
		return desiredFile{OID: e.OID, Mode: normalizeMode(e.Mode)}, true, nil
	}
	return desiredFile{}, false, nil
}

func formatOf(oid string) string {
	if len(oid) == 64 {
		return "sha256"
	}
	return "sha1"
}

// temporaryBranch returns a unique branch name graft/<user>/<random> for a
// commit that is not yet landed. The user is used verbatim when it is a valid
// reference name component, so that a policy written for the agent name, such as
// refs/heads/graft/build_bot/**, matches. Any other name, such as an author name
// with spaces, is turned into a lowercase slug of letters, digits and dashes.
func temporaryBranch(user string) (string, error) {
	owner := user
	if !validRefComponent(owner) {
		owner = refSlug(user)
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "graft/" + owner + "/" + hex.EncodeToString(random), nil
}

// validRefComponent reports whether s can be one component of a reference name
// as git check-ref-format defines it, and is also accepted as part of a revision
// by the server.
func validRefComponent(s string) bool {
	switch {
	case s == "", s == "@", !utf8.ValidString(s),
		strings.HasPrefix(s, "."), strings.HasSuffix(s, "."), strings.HasSuffix(s, ".lock"),
		strings.Contains(s, ".."), strings.Contains(s, "@{"),
		strings.ContainsAny(s, "/ ~^:?*[\\"):
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// refSlug turns a name into a lowercase slug of letters, digits and dashes. An
// empty slug becomes "agent".
func refSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "agent"
	}
	return slug
}
