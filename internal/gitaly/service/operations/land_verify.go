package operations

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
)

// This file is the optional verification of a resolution ([graft.land.resolver.verify], PLAN10
// 3.6): before a resolution lands, the configured command runs in a temporary checkout of the
// merged tree, the resolution included. A failure or a timeout rejects the resolution, and the
// conflict package says why. It applies to resolver answers and agents' resolutions alike.
//
// The checkout is the tree byte for byte (landCheckoutTree): what the operator's command sees is
// what lands. git-archive(1) is not used, since it honours the attributes of the tree itself
// (export-ignore hides a file, export-subst changes its content), and a .gitattributes written by
// an agent or by a model would decide what the verification sees.

// landVerifyOutputBytes bounds the output of the command kept as the reason.
const landVerifyOutputBytes = 4 << 10

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

	dir, err := os.MkdirTemp("", "graft-verify-")
	if err != nil {
		return fmt.Errorf("verification directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	checkout, home := filepath.Join(dir, "tree"), filepath.Join(dir, "home")
	for _, d := range []string{checkout, home} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return fmt.Errorf("verification directory: %w", err)
		}
	}

	if err := landCheckoutTree(ctx, repo, tree, checkout); err != nil {
		return fmt.Errorf("checking out the merged tree: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
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

// landVerifyUnfinishedError is a verification that could not be completed, because its context ended
// (the deadline of the land, or the caller) while it waited for a slot or ran: not a verdict.
type landVerifyUnfinishedError struct {
	err error
}

func (e *landVerifyUnfinishedError) Error() string {
	return "verification not finished: " + e.err.Error()
}

func (e *landVerifyUnfinishedError) Unwrap() error { return e.err }

// landVerifyOutOfLane runs the verification of a model's resolution out of the lane (PLAN10 15.9),
// with a slot of the verify pool. It returns a *landVerifyError when the command fails or times
// out, a *landVerifyUnfinishedError when ctx ended first, and another error when the verification
// could not be run.
func (s *Server) landVerifyOutOfLane(ctx context.Context, repo *localrepo.Repo, tree git.ObjectID) error {
	slot, err := s.conflictResolver.AcquireVerifySlot(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return &landVerifyUnfinishedError{err: err}
		}
		return err
	}
	defer slot.Release()

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
// gitlink (a submodule), by path. size and target are filled when it is written: the length of the
// file, the target of the link.
type landTreeEntry struct {
	mode   uint32
	oid    git.ObjectID
	path   string
	size   int64
	target string
}

const (
	landTreeModeTypeMask = 0o170000
	landTreeModeRegular  = 0o100000
	landTreeModeSymlink  = 0o120000
	landTreeModeGitlink  = 0o160000
	// landMaxLinkTarget bounds the target of a symbolic link, which is read into memory.
	landMaxLinkTarget = 64 << 10
)

func (e landTreeEntry) isGitlink() bool { return e.mode&landTreeModeTypeMask == landTreeModeGitlink }
func (e landTreeEntry) isSymlink() bool { return e.mode&landTreeModeTypeMask == landTreeModeSymlink }
func (e landTreeEntry) isRegular() bool { return e.mode&landTreeModeTypeMask == landTreeModeRegular }

// landCheckoutTree writes the tree under dir byte for byte: the entries of git-ls-tree(1) -r and the
// contents of git-cat-file(1) --batch, which apply no attribute (no export-ignore, export-subst,
// eol, filter or smudge), with the modes (a file is executable or not), the symbolic links as links,
// and the gitlinks as empty directories. It then reads the directory back and compares it with the
// listing: a difference fails the checkout, closed, since the operator's command would otherwise
// verify a tree that is not the one that lands.
func landCheckoutTree(ctx context.Context, repo *localrepo.Repo, tree git.ObjectID, dir string) error {
	return landCheckoutTreeThen(ctx, repo, tree, dir, nil)
}

// landCheckoutTreeThen is landCheckoutTree with a function that runs between the writing and the
// check; only tests give one, to damage the checkout and see that the check notices.
func landCheckoutTreeThen(ctx context.Context, repo *localrepo.Repo, tree git.ObjectID, dir string, afterWrite func()) error {
	var listing bytes.Buffer
	var stderr landBoundedBuffer
	stderr.limit = landVerifyOutputBytes
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{Name: "ls-tree", Flags: []gitcmd.Option{gitcmd.Flag{Name: "-r"}, gitcmd.Flag{Name: "-z"}, gitcmd.Flag{Name: "--full-tree"}}, Args: []string{tree.String()}},
		gitcmd.WithStdout(&listing),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return fmt.Errorf("listing the tree: %w, stderr: %q", err, stderr.String())
	}
	entries, err := landParseTreeListing(listing.Bytes())
	if err != nil {
		return err
	}

	// The contents come in the order of the listing, one blob after the other: the files and the links.
	var input bytes.Buffer
	for _, e := range entries {
		if !e.isGitlink() {
			input.WriteString(e.oid.String() + "\n")
		}
	}
	stderr = landBoundedBuffer{limit: landVerifyOutputBytes}
	cmd, err := repo.Exec(ctx,
		gitcmd.Command{Name: "cat-file", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--batch"}}},
		gitcmd.WithStdin(&input),
		gitcmd.WithSetupStdout(),
		gitcmd.WithStderr(&stderr),
	)
	if err != nil {
		return fmt.Errorf("reading the blobs of the tree: %w", err)
	}
	batch := bufio.NewReaderSize(cmd, 1<<20)
	writeErr := landMaterialize(entries, batch, dir)
	if writeErr == nil {
		// Nothing else may follow the last blob.
		if _, err := batch.ReadByte(); !errors.Is(err, io.EOF) {
			writeErr = fmt.Errorf("git-cat-file(1) answered more than the blobs asked for: %v", err)
		}
	}
	if waitErr := cmd.Wait(); waitErr != nil && writeErr == nil {
		return fmt.Errorf("reading the blobs of the tree: %w, stderr: %q", waitErr, stderr.String())
	}
	if writeErr != nil {
		return writeErr
	}
	if afterWrite != nil {
		afterWrite()
	}
	return landCheckMaterialized(entries, dir)
}

// landParseTreeListing parses the output of git-ls-tree(1) -r -z: "<mode> <type> <object>\t<path>"
// entries ended by NUL. A tree holds files, links and gitlinks once listed recursively.
func landParseTreeListing(raw []byte) ([]landTreeEntry, error) {
	var entries []landTreeEntry
	for _, record := range strings.Split(string(raw), "\x00") {
		if record == "" {
			continue
		}
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("unexpected entry in the listing of the tree: %q", record)
		}
		mode, err := strconv.ParseUint(fields[0], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("unexpected mode in the listing of the tree: %q", record)
		}
		e := landTreeEntry{mode: uint32(mode), oid: git.ObjectID(fields[2]), path: name}
		switch {
		case e.isRegular() && fields[1] == "blob", e.isSymlink() && fields[1] == "blob", e.isGitlink() && fields[1] == "commit":
		default:
			return nil, fmt.Errorf("entry %q has the mode %s and the type %s, which a checkout does not write", name, fields[0], fields[1])
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// landTreePathValid reports whether the path of a tree entry may be written under a directory: a
// clean relative path that does not leave it.
func landTreePathValid(name string) bool {
	return name != "" && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../") &&
		!strings.ContainsRune(name, 0)
}

// landMaterialize writes the entries under dir, reading the contents of the files and links from batch
// in their order (the answer of git-cat-file(1) --batch to the blobs of the listing, gitlinks left
// out). It refuses a path that would leave dir or lie under a symbolic link it wrote: a link is
// written as a link and never followed. It sets size and target of the entries.
func landMaterialize(entries []landTreeEntry, batch *bufio.Reader, dir string) error {
	links := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		if !landTreePathValid(e.path) {
			return fmt.Errorf("unsafe path %q in the tree", e.path)
		}
		for p := path.Dir(e.path); p != "."; p = path.Dir(p) {
			if links[p] {
				return fmt.Errorf("entry %q lies under the symbolic link %q", e.path, p)
			}
		}
		target := filepath.Join(dir, filepath.FromSlash(e.path))
		if e.isGitlink() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		size, err := landReadBatchHeader(batch, e.oid)
		if err != nil {
			return fmt.Errorf("%q: %w", e.path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if e.isSymlink() {
			if size > landMaxLinkTarget {
				return fmt.Errorf("the symbolic link %q has a target of %d bytes", e.path, size)
			}
			content := make([]byte, size)
			if _, err := io.ReadFull(batch, content); err != nil {
				return fmt.Errorf("%q: %w", e.path, err)
			}
			if err := os.Symlink(string(content), target); err != nil {
				return err
			}
			e.target, e.size = string(content), size
			links[e.path] = true
		} else {
			perm := os.FileMode(0o644)
			if e.mode&0o111 != 0 {
				perm = 0o755
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(f, batch, size); err != nil {
				_ = f.Close()
				return fmt.Errorf("%q: %w", e.path, err)
			}
			if err := f.Close(); err != nil {
				return err
			}
			// The mode is the tree's, whatever the umask of the process.
			if err := os.Chmod(target, perm); err != nil {
				return err
			}
			e.size = size
		}
		if b, err := batch.ReadByte(); err != nil || b != '\n' {
			return fmt.Errorf("%q: the blob is not followed by a line feed", e.path)
		}
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

// landCheckMaterialized reads dir back and compares it with the entries that were written: every entry
// is there with its type, its executable bit, the length of its file or the target of its link; a
// gitlink is an empty directory; and nothing else is there but the directories that hold the
// entries. A difference is an error.
func landCheckMaterialized(entries []landTreeEntry, dir string) error {
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
			if err != nil || info.Mode()&os.ModeSymlink == 0 || got != e.target {
				return fmt.Errorf("the symbolic link %q does not point to %q", name, e.target)
			}
		case isEntry && e.isRegular():
			if !info.Mode().IsRegular() || info.Size() != e.size || (info.Mode().Perm()&0o111 != 0) != (e.mode&0o111 != 0) {
				return fmt.Errorf("the file %q is not the file of the tree (size %d, executable %v)", name, e.size, e.mode&0o111 != 0)
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
