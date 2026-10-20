package operations

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // the hash git names its objects with
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// This file is the optional verification of a resolution ([graft.land.resolver.verify], PLAN10
// 3.6): before a resolution lands, the configured command runs in a temporary checkout of the
// merged tree, the resolution included. A failure or a timeout rejects the resolution, and the
// conflict package says why. It applies to resolver answers and agents' resolutions alike.
//
// The checkout is the tree byte for byte (landCheckoutTreeThen): what the operator's command sees is
// what lands. git-archive(1) is not used, since it honours the attributes of the tree itself
// (export-ignore hides a file, export-subst changes its content), and a .gitattributes written by
// an agent or by a model would decide what the verification sees.

// landVerifyOutputBytes bounds the output of the command kept as the reason.
const landVerifyOutputBytes = 4 << 10

// landVerifyCheckoutFaults counts the checkouts that failed on the server's own file system.
var landVerifyCheckoutFaults = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_land_verify_checkout_faults_total",
	Help: "Checkouts of a tree for the verification that failed on the file system of the server (no space, no descriptors, an I/O error, no permission).",
})

// landVerifyCleanupFailures counts the verification directories that could not be removed.
var landVerifyCleanupFailures = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_land_verify_cleanup_failures_total",
	Help: "Verification directories that could not be removed after the verification, even after making the tree writable.",
})

// landRemoveCheckout removes the directory of a verification. The operator's command runs in it and
// may leave directories that cannot be removed (read-only, no execute bit): the tree is made writable
// first, directory by directory before they are read. A directory that still cannot be removed is
// logged and counted, and leaves a trace in the log: it would otherwise fill the disk unseen.
func (s *Server) landRemoveCheckout(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil {
		landVerifyCleanupFailures.Inc()
		s.logger.WithError(err).WithField("directory", dir).Error("graft: a verification directory could not be removed")
	}
}

// landVerifyError is a resolution the verification rejected.
type landVerifyError struct {
	reason string
}

func (e *landVerifyError) Error() string {
	return "verification failed: " + e.reason
}

// landVerify runs the verification command on tree. It returns a *landVerifyError when the
// command fails or times out, and another error when the verification could not be run.
func (s *Server) landVerify(ctx context.Context, repo *localrepo.Repo, tree git.ObjectID) error {
	v := s.resolveCfg.Verify
	timeout, err := v.TimeoutOrDefault()
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp(s.verifyTmpDir, "graft-verify-")
	if err != nil {
		return fmt.Errorf("verification directory: %w", err)
	}
	defer s.landRemoveCheckout(dir)
	checkout, home := filepath.Join(dir, "tree"), filepath.Join(dir, "home")
	for _, d := range []string{checkout, home} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return fmt.Errorf("verification directory: %w", err)
		}
	}

	// The timeout bounds the checkout and the command together: a tree that takes the whole of it to
	// write is a verification that did not finish.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	limits := landCheckoutLimits{
		entries:   v.MaxTreeEntriesOrDefault(),
		bytes:     v.MaxTreeBytesOrDefault(),
		depth:     v.MaxTreeDepthOrDefault(),
		pathBytes: landMaxPathBytes,
	}
	if err := landCheckoutTreeThen(runCtx, repo, tree, checkout, limits, s.verifyAfterWriteHook); err != nil {
		var refusal *landCheckoutRefusal
		var fault *landCheckoutFault
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			return &landVerifyError{reason: fmt.Sprintf("the checkout of the tree and the command did not finish within %s", timeout)}
		case errors.As(err, &refusal):
			return &landVerifyError{reason: "nothing was verified, the merged tree is not checked out: " + refusal.reason}
		case errors.As(err, &fault):
			// The server's own file system failed: its log has the path, the agent gets none.
			s.logger.WithError(err).Error("graft: the checkout of a tree to verify failed on the file system of the server")
			landVerifyCheckoutFaults.Inc()
			return errors.New("checking out the merged tree failed on the file system of the server")
		}
		return fmt.Errorf("checking out the merged tree: %w", err)
	}
	var output landBoundedBuffer
	output.limit = landVerifyOutputBytes
	cmd, err := command.New(runCtx, s.logger, v.Command,
		command.WithDir(checkout),
		command.WithEnvironment([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home}),
		command.WithStdout(&output),
		command.WithStderr(&output),
	)
	if err != nil {
		return fmt.Errorf("starting the verification: %w", err)
	}
	runErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		// The caller's deadline or cancellation, not a verdict of the command.
		return ctx.Err()
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return &landVerifyError{reason: fmt.Sprintf("the command did not finish within %s", timeout)}
	case runErr != nil:
		reason := strings.TrimSpace(output.String())
		if code, ok := command.ExitStatus(runErr); ok {
			reason = fmt.Sprintf("exit status %d: %s", code, reason)
		}
		return &landVerifyError{reason: reason}
	}
	return nil
}

// landTipMovedError says that the branch moved while a verification waited for its turn: the tree
// was not verified, because it is not the tree the land would write any more.
type landTipMovedError struct{}

func (*landTipMovedError) Error() string {
	return "the branch moved while the verification waited for its turn"
}

// landVerifyUnfinishedError is a verification that could not be completed, because its context ended
// (the deadline of the land, or the caller) while it waited for a slot or ran: not a verdict.
type landVerifyUnfinishedError struct {
	err error
}

func (e *landVerifyUnfinishedError) Error() string {
	return "verification not finished: " + e.err.Error()
}

func (e *landVerifyUnfinishedError) Unwrap() error { return e.err }

// landVerifyRepoKey is the key of a repository in the verify pool: its storage and relative path.
func landVerifyRepoKey(repository *gitalypb.Repository) string {
	return repository.GetStorageName() + "\x00" + repository.GetRelativePath()
}

// landVerifyOutOfLane runs the verification of a resolution out of the lane (PLAN10 15.9), whoever made it,
// with a slot of the verify pool. It returns a *landVerifyError when the command fails or times
// out, a *landVerifyUnfinishedError when ctx ended first, and another error when the verification
// could not be run.
func (s *Server) landVerifyOutOfLane(ctx context.Context, repository *gitalypb.Repository, repo *localrepo.Repo, tree git.ObjectID, current func(context.Context) (bool, error)) error {
	slot, err := s.verifyPool.Acquire(ctx, landVerifyRepoKey(repository))
	if err != nil {
		if ctx.Err() != nil {
			return &landVerifyUnfinishedError{err: err}
		}
		return err
	}
	defer slot.Release()
	// The wait for a slot can take as long as the verification before it: the tree was built from a
	// tip that the branch may have left meanwhile. Nothing is verified that is not current.
	if current != nil {
		ok, err := current(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return &landVerifyUnfinishedError{err: err}
		case err != nil:
			return err
		case !ok:
			return &landTipMovedError{}
		}
	}

	err = s.landVerify(ctx, repo, tree)
	var verifyErr *landVerifyError
	switch {
	case err == nil, errors.As(err, &verifyErr):
		return err
	case ctx.Err() != nil:
		return &landVerifyUnfinishedError{err: err}
	}
	return err
}

// landVerifyTrailer is what Graft-Checked-By says of a verification that passed: verify:<command>,
// the base name of the command, or verify when that is not a valid trailer value.
func (s *Server) landVerifyTrailer() string {
	name := "verify"
	if cmd := s.resolveCfg.Verify.Command; len(cmd) > 0 {
		if v := "verify:" + filepath.Base(cmd[0]); landTrailerValueValid(v) {
			name = v
		}
	}
	return name
}

// landTreeEntry is one entry of a tree as git-ls-tree(1) -r lists it: a file, a symbolic link or a
// gitlink (a submodule), by path. size is the length of the file once it is written. The target of a
// link is not kept: the read-back hashes what the link says and compares it with the OID. sized and
// listed are the size that the listing gave (ls-tree -l), when it did.
type landTreeEntry struct {
	mode   uint32
	oid    git.ObjectID
	path   string
	size   int64
	sized  bool
	listed int64
}

const (
	landTreeModeTypeMask = 0o170000
	landTreeModeRegular  = 0o100000
	landTreeModeSymlink  = 0o120000
	landTreeModeGitlink  = 0o160000
	// landMaxLinkTarget is the longest target of a symbolic link that Linux takes (PATH_MAX - 1): a
	// longer one is refused up front, with the reason. Nothing of it is kept after the link is made.
	landMaxLinkTarget = 4095
	// landMaxTreePath bounds a path of the tree, and with it the record that is read of the listing.
	landMaxTreePath = 4096
	// landNameMax and landPathMax are the limits of a name and of a whole path on the file systems
	// of Linux (NAME_MAX, PATH_MAX): beyond them the checkout refuses, rather than fail on the system call.
	landNameMax = 255
	landPathMax = 4096
)

func (e landTreeEntry) isGitlink() bool { return e.mode&landTreeModeTypeMask == landTreeModeGitlink }
func (e landTreeEntry) isSymlink() bool { return e.mode&landTreeModeTypeMask == landTreeModeSymlink }
func (e landTreeEntry) isRegular() bool { return e.mode&landTreeModeTypeMask == landTreeModeRegular }

// landMaxPathBytes bounds the memory a checkout keeps for the paths it has seen, counted as the
// materializer charges it, exactly: an entry costs its path plus landEntryOverhead (the entries kept
// for the read-back); every distinct prefix of a path in the set of folded names costs twice its
// length (the folded key and the spelling) plus landKeyOverhead; every directory made costs its path
// plus landKeyOverhead (the set of directories); every symbolic link costs its path plus
// landKeyOverhead (the set of links). Nothing else of the tree is kept: not the target of a link, not
// the content of a file. A few thousand bytes of path times many entries is where a small push could
// otherwise take the memory of the server.
const (
	landMaxPathBytes  = 128 << 20
	landEntryOverhead = 256
	landKeyOverhead   = 64
)

// landCheckoutLimits bound a checkout; a zero field is no limit (the server always sets them, from
// verify.max_tree_entries, verify.max_tree_bytes, verify.max_tree_depth and landMaxPathBytes).
//
// entries counts the entries of the tree AND the directories the checkout makes for them, as both
// are inodes; depth is the number of components of a path; bytes the bytes of the files and links
// (a link's target included); pathBytes the memory of the paths.
type landCheckoutLimits struct {
	entries   int
	bytes     int64
	depth     int
	pathBytes int64
}

// landCheckoutRefusal is a tree that the checkout refuses to write, closed: it holds more than the
// limits allow, or a path that is not safe to write under a directory. It is no failure of the
// server: the command does not run, and the reason goes to the agent in the conflict package.
type landCheckoutRefusal struct {
	reason string
}

func (e *landCheckoutRefusal) Error() string { return e.reason }

func landRefuse(format string, args ...any) error {
	return &landCheckoutRefusal{reason: fmt.Sprintf(format, args...)}
}

// landCheckoutTreeThen writes the tree under dir byte for byte: the entries of git-ls-tree(1) -r and the
// contents of git-cat-file(1) --batch, which apply no attribute (no export-ignore, export-subst,
// eol, filter or smudge), with the modes (a file is executable or not), the symbolic links as links,
// and the gitlinks as empty directories. It then reads the directory back and compares it with the
// listing, the content of every file included (the blob hash of what was written must be the OID of
// the tree): a difference fails the checkout, closed, since the operator's command would otherwise
// verify a tree that is not the one that lands.
//
// The listing and the blobs are streamed, one entry at a time, so the memory it takes is that of the
// entries (which limits bounds) and not of the tree. A tree beyond the limits, or with a path that is
// not safe to write, is a *landCheckoutRefusal.
//
// afterWrite, when given, runs between the writing and the check; only tests give one, to damage the
// checkout and see that the check notices.
func landCheckoutTreeThen(ctx context.Context, repo *localrepo.Repo, tree git.ObjectID, dir string, limits landCheckoutLimits, afterWrite func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var lsStderr, catStderr landBoundedBuffer
	lsStderr.limit, catStderr.limit = landVerifyOutputBytes, landVerifyOutputBytes
	ls, err := repo.Exec(ctx,
		gitcmd.Command{Name: "ls-tree", Flags: []gitcmd.Option{gitcmd.Flag{Name: "-r"}, gitcmd.Flag{Name: "-z"}, gitcmd.Flag{Name: "-l"}, gitcmd.Flag{Name: "--full-tree"}}, Args: []string{tree.String()}},
		gitcmd.WithSetupStdout(),
		gitcmd.WithStderr(&lsStderr),
	)
	if err != nil {
		return fmt.Errorf("listing the tree: %w", err)
	}
	// The blobs are asked for through a pipe, as the listing is read: the contents come in the order
	// of the listing, one blob after the other, files and links, gitlinks left out.
	pipeR, pipeW := io.Pipe()
	cat, err := repo.Exec(ctx,
		gitcmd.Command{Name: "cat-file", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--batch"}}},
		gitcmd.WithStdin(pipeR),
		gitcmd.WithSetupStdout(),
		gitcmd.WithStderr(&catStderr),
	)
	if err != nil {
		cancel()
		_ = ls.Wait()
		return fmt.Errorf("reading the blobs of the tree: %w", err)
	}

	listed := make(chan landTreeEntry, 256)
	fed := make(chan error, 1)
	go func() {
		err := landFeed(ctx, bufio.NewReaderSize(ls, 1<<20), pipeW, listed)
		close(listed)
		_ = pipeW.CloseWithError(err) // nil: the end of the input, and cat-file ends
		fed <- err
	}()

	var entries []landTreeEntry
	m := newLandMaterializer(ctx, dir, limits)
	batch := bufio.NewReaderSize(cat, 1<<20)
	var writeErr error
	for e := range listed {
		entries = append(entries, e)
		if writeErr = m.add(&entries[len(entries)-1], batch); writeErr != nil {
			break
		}
	}
	if writeErr != nil {
		// Let the feeder and the commands go: none of them is needed any more.
		cancel()
		_ = pipeR.Close()
	}
	feedErr := <-fed
	if writeErr == nil && feedErr == nil {
		// Nothing else may follow the last blob.
		if _, err := batch.ReadByte(); !errors.Is(err, io.EOF) {
			writeErr = fmt.Errorf("git-cat-file(1) answered more than the blobs asked for: %v", err)
		}
	}
	lsWait, catWait := ls.Wait(), cat.Wait()
	switch {
	case writeErr != nil:
		return writeErr
	case feedErr != nil:
		return feedErr
	case lsWait != nil:
		return fmt.Errorf("listing the tree: %w, stderr: %q", lsWait, lsStderr.String())
	case catWait != nil:
		return fmt.Errorf("reading the blobs of the tree: %w, stderr: %q", catWait, catStderr.String())
	}
	if afterWrite != nil {
		afterWrite()
	}
	return landCheckMaterialized(ctx, entries, dir)
}

// landFeed reads the entries of the listing (ls-tree -r -z -l), asks cat-file for the blob of each
// that is not a gitlink, one line per object ID, and hands the entries on, in order.
func landFeed(ctx context.Context, listing *bufio.Reader, oids io.Writer, out chan<- landTreeEntry) error {
	for {
		record, err := landReadRecord(listing)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		e, err := landParseTreeRecord(record)
		if err != nil {
			return err
		}
		if !e.isGitlink() {
			if _, err := io.WriteString(oids, e.oid.String()+"\n"); err != nil {
				return fmt.Errorf("asking for the blobs of the tree: %w", err)
			}
		}
		select {
		case out <- e:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// landReadRecord reads one NUL-ended record, of at most landMaxTreePath plus the bytes of its
// mode, type, object ID and size. io.EOF means the listing ended between two records.
func landReadRecord(r *bufio.Reader) (string, error) {
	var record []byte
	for {
		chunk, err := r.ReadSlice(0)
		record = append(record, chunk...)
		if len(record) > landMaxTreePath+256 {
			return "", landRefuse("the tree has a path longer than %d bytes", landMaxTreePath)
		}
		switch {
		case err == nil:
			return string(record[:len(record)-1]), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(record) == 0:
			return "", io.EOF
		case errors.Is(err, io.EOF):
			return "", fmt.Errorf("the listing of the tree ends in the middle of an entry: %w", io.ErrUnexpectedEOF)
		}
		return "", fmt.Errorf("reading the listing of the tree: %w", err)
	}
}

// landParseTreeRecord parses one entry of the listing.
func landParseTreeRecord(record string) (landTreeEntry, error) {
	meta, name, ok := strings.Cut(record, "\t")
	fields := strings.Fields(meta)
	if !ok || (len(fields) != 3 && len(fields) != 4) {
		return landTreeEntry{}, fmt.Errorf("unexpected entry in the listing of the tree: %q", record)
	}
	mode, err := strconv.ParseUint(fields[0], 8, 32)
	if err != nil {
		return landTreeEntry{}, fmt.Errorf("unexpected mode in the listing of the tree: %q", record)
	}
	if !landObjectIDValid(fields[2]) {
		return landTreeEntry{}, fmt.Errorf("unexpected object ID in the listing of the tree: %q", record)
	}
	e := landTreeEntry{mode: uint32(mode), oid: git.ObjectID(fields[2]), path: name}
	if len(fields) == 4 && fields[3] != "-" {
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return landTreeEntry{}, fmt.Errorf("unexpected size in the listing of the tree: %q", record)
		}
		e.sized, e.listed = true, size
	}
	switch {
	case e.isRegular() && fields[1] == "blob", e.isSymlink() && fields[1] == "blob", e.isGitlink() && fields[1] == "commit":
	default:
		return landTreeEntry{}, fmt.Errorf("entry %q has the mode %s and the type %s, which a checkout does not write", name, fields[0], fields[1])
	}
	return e, nil
}

// landObjectIDValid reports whether s is an object ID: 40 (SHA-1) or 64 (SHA-256) lower-case hex digits.
func landObjectIDValid(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// landTreePathValid reports whether the path of a tree entry may be written under a directory: a
// clean relative path that does not leave it.
func landTreePathValid(name string) bool {
	return name != "" && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../") &&
		!strings.ContainsRune(name, 0)
}

// landFolded is what a case-insensitive file system sees at a path: its first spelling, and whether
// that is a symbolic link.
type landFolded struct {
	spelling string
	link     bool
}

// landMaterializer writes the entries of a tree under dir, one at a time.
type landMaterializer struct {
	ctx    context.Context
	dir    string
	limits landCheckoutLimits
	links  map[string]bool       // the paths of the symbolic links written
	dirs   map[string]bool       // the directories made
	folded map[string]landFolded // the paths as a case-insensitive file system sees them
	count  int                   // entries written, and directories made
	bytes  int64                 // bytes of files and links written
	paths  int64                 // memory of the paths kept, as landMaxPathBytes counts it
}

func newLandMaterializer(ctx context.Context, dir string, limits landCheckoutLimits) *landMaterializer {
	return &landMaterializer{ctx: ctx, dir: dir, limits: limits, links: map[string]bool{}, dirs: map[string]bool{}, folded: map[string]landFolded{}}
}

// landCheckoutFault is a failure of the server's own file system while it wrote the tree (no space,
// no descriptors, an I/O error, no permission): not the tree's doing, so no refusal and no proposal,
// an error of the server with a log and a metric.
type landCheckoutFault struct {
	err error
}

func (e *landCheckoutFault) Error() string {
	return "checkout failed on the file system: " + e.err.Error()
}
func (e *landCheckoutFault) Unwrap() error { return e.err }

// osError sorts what the file system said. What the TREE causes is a refusal with its reason: a name
// that exists already (two names a case-insensitive file system makes one), a name or a path too
// long, an illegal byte sequence, a link in the way, a file where a directory is needed. What the
// SERVER's file system causes is a fault: no space, no descriptors, an I/O error, no permission. The
// reason names the path relative to the checkout, never the directory of the server.
func (m *landMaterializer) osError(err error) error {
	if err == nil {
		return nil
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EEXIST, syscall.ENAMETOOLONG, syscall.EILSEQ, syscall.ELOOP, syscall.ENOTDIR, syscall.EISDIR, syscall.ENOTEMPTY, syscall.EINVAL, syscall.EMLINK:
			return landRefuse("the tree cannot be written on this file system: %s", m.describe(err, errno))
		}
	}
	return &landCheckoutFault{err: err}
}

// describe words a system error with the operation and the path relative to the checkout.
func (m *landMaterializer) describe(err error, errno syscall.Errno) string {
	op, p := "", ""
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		op, p = pe.Op, pe.Path
	case errors.As(err, &le):
		op, p = le.Op, le.New
	}
	rel, ok := strings.CutPrefix(p, m.dir+string(filepath.Separator))
	if !ok {
		return strings.TrimSpace(op + ": " + errno.Error())
	}
	return op + " " + rel + ": " + errno.Error()
}

// landOSWriter sorts the errors of writing a file, and leaves the errors of reading git alone.
type landOSWriter struct {
	m *landMaterializer
	w io.Writer
}

func (o landOSWriter) Write(p []byte) (int, error) {
	n, err := o.w.Write(p)
	return n, o.m.osError(err)
}

// add writes one entry. It refuses, with a *landCheckoutRefusal:
//   - a path that is not clean, or leaves dir;
//   - a path longer than landMaxTreePath, or that with dir reaches PATH_MAX; a name over 255 bytes;
//   - more entries and directories, more bytes or components than the limits allow;
//   - a component that names .git on some file system (resolver.IsGitDirName): the operator's
//     command may run git in the checkout, and a .git of the tree would be its repository, whose
//     configuration and hooks run;
//   - an entry under a symbolic link that was written, also when a case-insensitive file system
//     would see the same name (a link "A" and an entry "a/x");
//   - a parent that is not a directory; a target of a link over 4095 bytes or with a NUL in it.
//
// Each directory is made one component at a time and looked at with Lstat, never with MkdirAll,
// which would follow a link; a file is created with O_EXCL and O_NOFOLLOW; a link is written as a
// link and never followed. The memory it keeps for the paths is charged before it is taken
// (landMaxPathBytes).
func (m *landMaterializer) add(e *landTreeEntry, batch *bufio.Reader) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	if !landTreePathValid(e.path) {
		return landRefuse("unsafe path %q in the tree", e.path)
	}
	// The file system's own limits, refused up front: a name of more than 255 bytes, a path that
	// with the directory of the checkout reaches PATH_MAX.
	if len(e.path) > landMaxTreePath || len(m.dir)+1+len(e.path) >= landPathMax {
		return landRefuse("the tree has a path too long to write (limit %d bytes, with the directory of the checkout)", min(landMaxTreePath, landPathMax-len(m.dir)-2))
	}
	if err := m.charge(1); err != nil {
		return err
	}
	if e.sized && !e.isGitlink() && m.limits.bytes > 0 && m.bytes+e.listed > m.limits.bytes {
		return landRefuse("the tree has more than %d bytes of files (verify.max_tree_bytes)", m.limits.bytes)
	}
	if e.isSymlink() && e.sized && e.listed == 0 {
		return landRefuse("the symbolic link %q has an empty target, which no file system takes", e.path)
	}
	if e.isSymlink() && e.sized && e.listed > landMaxLinkTarget {
		return landRefuse("the symbolic link %q has a target of %d bytes, more than the %d that Linux takes", e.path, e.listed, landMaxLinkTarget)
	}

	if m.limits.depth > 0 {
		if depth := strings.Count(e.path, "/") + 1; depth > m.limits.depth {
			return landRefuse("the tree has a path of %d components, more than %d (verify.max_tree_depth)", depth, m.limits.depth)
		}
	}
	for rest := e.path; rest != ""; {
		c, tail, _ := strings.Cut(rest, "/")
		rest = tail
		if len(c) > landNameMax {
			return landRefuse("the path %q has a name of more than %d bytes", e.path, landNameMax)
		}
		if resolver.IsGitDirName(c) {
			return landRefuse("the path %q has a component that names .git, which a checkout does not write", e.path)
		}
	}
	for p := path.Dir(e.path); p != "."; p = path.Dir(p) {
		if m.links[p] {
			return landRefuse("entry %q lies under the symbolic link %q", e.path, p)
		}
	}
	if err := m.chargePath(int64(len(e.path)) + landEntryOverhead); err != nil {
		return err
	}

	// The names as a case-insensitive file system sees them: a collision with a link is refused here,
	// and the new names are charged now but entered only after the directories are made and charged.
	type foldedName struct {
		key string
		landFolded
	}
	var fresh []foldedName
	for end := 0; end < len(e.path); {
		next := strings.IndexByte(e.path[end:], '/')
		if next < 0 {
			end = len(e.path)
		} else {
			end += next
		}
		prefix := e.path[:end]
		link := end == len(e.path) && e.isSymlink()
		f := resolver.FoldGitName(prefix)
		prev, seen := m.folded[f]
		switch {
		case !seen:
			if err := m.chargePath(2*int64(len(prefix)) + landKeyOverhead); err != nil {
				return err
			}
			fresh = append(fresh, foldedName{key: f, landFolded: landFolded{spelling: prefix, link: link}})
		case prev.spelling != prefix && (prev.link || link):
			return landRefuse("%q and %q are one name on a case-insensitive file system, and one is a symbolic link", e.path, prev.spelling)
		}
		end++
	}
	enter := func() {
		for _, n := range fresh {
			m.folded[n.key] = n.landFolded
		}
	}

	dir, _ := path.Split(e.path)
	dir = strings.TrimSuffix(dir, "/")
	if e.isGitlink() {
		if err := m.mkdirAll(e.path); err != nil {
			return err
		}
		enter()
		return nil
	}
	if m.dirs[e.path] {
		return landRefuse("the entry %q is also a directory of the tree", e.path)
	}
	size, err := landReadBatchHeader(batch, e.oid)
	if err != nil {
		return fmt.Errorf("%q: %w", e.path, err)
	}
	if e.sized && e.listed != size {
		return fmt.Errorf("%q: the listing says %d bytes and git-cat-file(1) %d", e.path, e.listed, size)
	}
	if m.bytes += size; m.limits.bytes > 0 && m.bytes > m.limits.bytes {
		return landRefuse("the tree has more than %d bytes of files (verify.max_tree_bytes)", m.limits.bytes)
	}
	if err := m.mkdirAll(dir); err != nil {
		return err
	}
	enter()
	target := filepath.Join(m.dir, filepath.FromSlash(e.path))
	if e.isSymlink() {
		if size == 0 {
			return landRefuse("the symbolic link %q has an empty target, which no file system takes", e.path)
		}
		if size > landMaxLinkTarget {
			return landRefuse("the symbolic link %q has a target of %d bytes, more than the %d that Linux takes", e.path, size, landMaxLinkTarget)
		}
		content := make([]byte, size)
		if _, err := io.ReadFull(batch, content); err != nil {
			return fmt.Errorf("%q: %w", e.path, err)
		}
		if bytes.IndexByte(content, 0) >= 0 {
			return landRefuse("the symbolic link %q has a NUL in its target", e.path)
		}
		if err := m.chargePath(int64(len(e.path)) + landKeyOverhead); err != nil {
			return err
		}
		if err := os.Symlink(string(content), target); err != nil {
			return m.osError(err)
		}
		e.size = size
		m.links[e.path] = true
	} else {
		perm := os.FileMode(0o644)
		if e.mode&0o111 != 0 {
			perm = 0o755
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
		if err != nil {
			return m.osError(err)
		}
		if _, err := io.CopyN(landOSWriter{m: m, w: f}, batch, size); err != nil {
			_ = f.Close()
			var refusal *landCheckoutRefusal
			var fault *landCheckoutFault
			if errors.As(err, &refusal) || errors.As(err, &fault) {
				return err
			}
			return fmt.Errorf("%q: %w", e.path, err)
		}
		if err := f.Close(); err != nil {
			return m.osError(err)
		}
		// The mode is the tree's, whatever the umask of the process.
		if err := os.Chmod(target, perm); err != nil {
			return m.osError(err)
		}
		e.size = size
	}
	if b, err := batch.ReadByte(); err != nil || b != '\n' {
		return fmt.Errorf("%q: the blob is not followed by a line feed", e.path)
	}
	return nil
}

// charge counts n more entries or directories against verify.max_tree_entries.
func (m *landMaterializer) charge(n int) error {
	if m.count += n; m.limits.entries > 0 && m.count > m.limits.entries {
		return landRefuse("the tree has more than %d entries and directories (verify.max_tree_entries)", m.limits.entries)
	}
	return nil
}

// chargePath counts n bytes of the memory kept for paths against landMaxPathBytes.
func (m *landMaterializer) chargePath(n int64) error {
	if m.paths += n; m.limits.pathBytes > 0 && m.paths > m.limits.pathBytes {
		return landRefuse("the paths of the tree take more than %d bytes of memory", m.limits.pathBytes)
	}
	return nil
}

// mkdirAll makes the directory rel (slash-separated, relative to dir, empty for dir itself), one
// component at a time. A component that exists must be a directory by Lstat, which does not follow a
// link: a link, or a file, in the way is a refusal. Each directory made is charged first.
func (m *landMaterializer) mkdirAll(rel string) error {
	for end := 0; end < len(rel); {
		next := strings.IndexByte(rel[end:], '/')
		if next < 0 {
			end = len(rel)
		} else {
			end += next
		}
		key := rel[:end]
		end++
		if m.dirs[key] {
			continue
		}
		current := filepath.Join(m.dir, filepath.FromSlash(key))
		info, err := os.Lstat(current)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// A directory is an inode and a path kept in memory, charged before it is made.
			if err := m.charge(1); err != nil {
				return err
			}
			if err := m.chargePath(int64(len(key)) + landKeyOverhead); err != nil {
				return err
			}
			if err := os.Mkdir(current, 0o755); err != nil {
				return m.osError(err)
			}
		case err != nil:
			return m.osError(err)
		case !info.IsDir():
			return landRefuse("%q is in the way of a directory of the tree, and is not a directory", key)
		}
		m.dirs[key] = true
	}
	return nil
}

// landReadBatchHeader reads the header "<object> <type> <size>" of an answer of git-cat-file(1) --batch
// and returns the size of the blob; the object must be the one asked for.
func landReadBatchHeader(batch *bufio.Reader, want git.ObjectID) (int64, error) {
	line, err := batch.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("reading the header of the blob: %w", err)
	}
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != want.String() || fields[1] != "blob" {
		return 0, fmt.Errorf("unexpected answer of git-cat-file(1) for %s: %q", want, strings.TrimSpace(line))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("unexpected size in the answer of git-cat-file(1): %q", strings.TrimSpace(line))
	}
	return size, nil
}

// landBlobOID is the object ID that git gives the blob that r holds, size bytes long: the hash of
// "blob <size>\0" and the content, with the hash of the repository (SHA-256 for an ID of 64 digits).
func landBlobOID(r io.Reader, size int64, like git.ObjectID) (git.ObjectID, error) {
	h := sha1.New() //nolint:gosec // the hash git names its objects with
	if len(like) == 64 {
		h = sha256.New()
	}
	fmt.Fprintf(h, "blob %d\x00", size)
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return git.ObjectID(hex.EncodeToString(h.Sum(nil))), nil
}

// landCtxReader stops a read when its context has ended, so that the hashing of a large tree is
// bounded by the deadline too.
type landCtxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c landCtxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// landCheckMaterialized reads dir back and compares it with the entries that were written: every entry
// is there with its type and its executable bit; the content of every file and the target of every
// link hashes, as a git blob, to the object ID of the tree (the target of a link is not kept: what
// the link says now is hashed); a gitlink is an empty directory; and nothing else is there but the
// directories that hold the entries. A difference is an error. The context is looked at at every
// entry and while a file is hashed: when it ends, the error is the context's, not a difference.
func landCheckMaterialized(ctx context.Context, entries []landTreeEntry, dir string) error {
	want := make(map[string]*landTreeEntry, len(entries))
	parents := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		want[e.path] = e
		for p := path.Dir(e.path); p != "."; p = path.Dir(p) {
			parents[p] = true
		}
	}
	seen := 0
	err := filepath.WalkDir(dir, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, full)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		e, isEntry := want[name]
		switch {
		case isEntry && e.isGitlink():
			if !info.IsDir() {
				return fmt.Errorf("the gitlink %q is not a directory", name)
			}
		case isEntry && e.isSymlink():
			got, err := os.Readlink(full)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("%q is not the symbolic link of the tree", name)
			}
			if oid, err := landBlobOID(strings.NewReader(got), int64(len(got)), e.oid); err != nil || oid != e.oid {
				return fmt.Errorf("the symbolic link %q is not the blob %s of the tree", name, e.oid)
			}
		case isEntry && e.isRegular():
			if !info.Mode().IsRegular() || info.Size() != e.size || (info.Mode().Perm()&0o111 != 0) != (e.mode&0o111 != 0) {
				return fmt.Errorf("the file %q is not the file of the tree (size %d, executable %v)", name, e.size, e.mode&0o111 != 0)
			}
			f, err := os.Open(full)
			if err != nil {
				return err
			}
			oid, err := landBlobOID(landCtxReader{ctx: ctx, r: f}, e.size, e.oid)
			_ = f.Close()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil || oid != e.oid {
				return fmt.Errorf("the content of the file %q is not the blob %s of the tree", name, e.oid)
			}
		case isEntry:
			return fmt.Errorf("unexpected entry %q", name)
		case d.IsDir() && parents[name]:
			return nil
		default:
			return fmt.Errorf("%q is in the checkout and not in the tree", name)
		}
		seen++
		return nil
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return fmt.Errorf("the checkout differs from the tree: %w", err)
	}
	if seen != len(entries) {
		return fmt.Errorf("the checkout differs from the tree: %d of %d entries are there", seen, len(entries))
	}
	return nil
}

// landBoundedBuffer keeps the first limit bytes written to it and drops the rest, so that a
// command cannot fill the server's memory with its output.
type landBoundedBuffer struct {
	buf   []byte
	limit int
}

func (b *landBoundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (b *landBoundedBuffer) String() string {
	return strings.ToValidUTF8(string(b.buf), "")
}
