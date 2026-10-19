package operations

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
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

	archive, err := os.Create(filepath.Join(dir, "tree.tar"))
	if err != nil {
		return fmt.Errorf("verification archive: %w", err)
	}
	defer archive.Close()
	var stderr landBoundedBuffer
	stderr.limit = landVerifyOutputBytes
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{Name: "archive", Flags: []gitcmd.Option{gitcmd.ValueFlag{Name: "--format", Value: "tar"}}, Args: []string{tree.String()}},
		gitcmd.WithStdout(archive),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return fmt.Errorf("archiving the merged tree: %w, stderr: %q", err, stderr.String())
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("verification archive: %w", err)
	}
	if err := landExtractTar(archive, checkout); err != nil {
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

// landExtractTar writes the files of the archive of a tree under dir. It refuses an entry that
// would leave dir, or that lies under a symbolic link it wrote: a link is written as a link and
// never followed.
func landExtractTar(r io.Reader, dir string) error {
	links := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue // git archive's commit header
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe archive entry %q", h.Name)
		}
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			if links[p] {
				return fmt.Errorf("archive entry %q lies under the symbolic link %q", h.Name, p)
			}
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
			links[name] = true
		default:
			return fmt.Errorf("archive entry %q has the unexpected type %q", h.Name, h.Typeflag)
		}
	}
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
