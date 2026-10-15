package diff

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/helper"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

func (s *server) GetPatchID(ctx context.Context, in *gitalypb.GetPatchIDRequest) (*gitalypb.GetPatchIDResponse, error) {
	if err := validatePatchIDRequest(ctx, s.locator, in); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	var diffCmdStderr strings.Builder

	repo := s.localRepoFactory.Build(in.GetRepository())

	diffFlags := []gitcmd.Option{
		// git-patch-id(1) will ignore binary diffs, and computing binary
		// diffs would be expensive anyway for large blobs. This means that
		// we must instead use the pre- and post-image blob IDs that
		// git-diff(1) prints for binary diffs as input to git-patch-id(1),
		// but unfortunately this is only honored in Git v2.39.0 and newer.
		// We have no other choice than to accept this though, so we instead
		// just ask git-diff(1) to print the full blob IDs for the pre- and
		// post-image blobs instead of abbreviated ones so that we can avoid
		// any kind of potential prefix collisions.
		gitcmd.Flag{Name: "--full-index"},
	}

	if in.GetContextFree() {
		diffFlags = append(diffFlags, gitcmd.Flag{Name: "-U0"})
	}

	diffCmd, err := repo.Exec(ctx,
		gitcmd.Command{
			Name:  "diff",
			Args:  []string{string(in.GetOldRevision()), string(in.GetNewRevision())},
			Flags: diffFlags,
		},
		gitcmd.WithStderr(&diffCmdStderr),
		gitcmd.WithSetupStdout(),
	)
	if err != nil {
		return nil, structerr.New("spawning diff: %w", err)
	}

	// patchID stays empty when the diff turns out to be empty.
	var patchID string

	if in.GetWholeDiffHash() {
		id, _, err := hashDiff(diffCmd)
		if err != nil {
			return nil, structerr.New("hashing diff: %w", err)
		}

		patchID = id
	} else {
		var patchIDStdout strings.Builder
		var patchIDStderr strings.Builder

		patchIDCmd, err := s.gitCmdFactory.NewWithoutRepo(ctx,
			gitcmd.Command{
				Name:  "patch-id",
				Flags: []gitcmd.Option{gitcmd.Flag{Name: "--verbatim"}},
			},
			gitcmd.WithStdin(diffCmd),
			gitcmd.WithStdout(&patchIDStdout),
			gitcmd.WithStderr(&patchIDStderr),
		)
		if err != nil {
			return nil, structerr.New("spawning patch-id: %w", err)
		}

		if err := patchIDCmd.Wait(); err != nil {
			return nil, structerr.New("waiting for patch-id: %w", err).WithMetadata("stderr", patchIDStderr.String())
		}

		if patchIDStdout.Len() > 0 {
			// When computing patch IDs for commits directly via e.g. `git show | git patch-id` then
			// the second field printed by git-patch-id(1) denotes the commit of the patch ID. As we
			// only generate patch IDs from a diff here the second field will always be the zero OID,
			// so we ignore it.
			id, _, found := strings.Cut(patchIDStdout.String(), " ")
			if !found {
				return nil, structerr.NewFailedPrecondition("unexpected patch ID format")
			}

			patchID = id
		}
	}

	if err := diffCmd.Wait(); err != nil {
		return nil, structerr.New("waiting for git-diff: %w", err).WithMetadata("stderr", diffCmdStderr.String())
	}

	if patchID == "" {
		return nil, structerr.NewFailedPrecondition("no difference between old and new revision")
	}

	return &gitalypb.GetPatchIDResponse{PatchId: patchID}, nil
}

// hashDiff computes a patch ID for a git-diff(1) patch by folding its normalized form into a single
// SHA256. It returns the hex-encoded digest along with the number of bytes that were hashed. An empty
// patch has no patch ID, so the digest is the empty string rather than the SHA256 of no input, which
// keeps the empty-diff handling identical to the git-patch-id(1) path.
//
// git-patch-id(1) instead finalizes one digest per file and adds those digests together with carry
// (diff.c:flush_one_hunk(), which despite its name runs once per file rather than once per hunk).
// That sum exists only to make the patch ID independent of the order in which the file diffs appear,
// which matters when comparing the same two trees with different -O<orderfile> settings. Gitaly never
// passes -O<orderfile>, so git-diff(1) output is always ordered by path and the property is worthless
// to us, whereas summing digests is inherently weaker than hashing the stream once.
func hashDiff(r io.Reader) (string, int, error) {
	hash := sha256.New()

	hashedBytes, err := normalizeDiff(hash, r)
	if err != nil {
		return "", 0, err
	}

	if hashedBytes == 0 {
		return "", 0, nil
	}

	return hex.EncodeToString(hash.Sum(nil)), hashedBytes, nil
}

// normalizeDiff writes the hashable form of a git-diff(1) patch to w and returns the number of bytes
// written, mirroring the normalization that builtin/patch-id.c:get_one_patchid() performs when
// git-patch-id(1) is passed `--verbatim`, with one deliberate exception documented below.
//
// Whitespace is preserved, while hunk headers and "index" lines are left out so that neither line
// offsets nor the blob IDs of the pre- and post-image contribute to the patch ID. Those two omissions
// are what let a patch ID survive a rebase onto a base that has moved.
//
// git additionally tracks how many pre- and post-image lines each hunk still owes it, so that it can
// tell a genuine header apart from a hunk body line that looks like one. Every body line in
// git-diff(1) output is prefixed with '+', '-' or ' ', so that ambiguity cannot arise here and those
// counters are left out. They would have to come back if this ever consumed git-format-patch(1) or
// mailbox input, where an unprefixed line of a commit message can look exactly like a diff header.
func normalizeDiff(w io.Writer, r io.Reader) (int, error) {
	// Lines are read with ReadSlice(3) instead of being scanned or copied out: a single line of a
	// minified file readily exceeds the maximum token size that bufio.Scanner is willing to return,
	// and ReadBytes(3) would allocate a fresh copy of every line only for it to be discarded right
	// after hashing. An over-long line arrives as multiple fragments, all sharing the write-or-skip
	// decision made for its first fragment.
	reader := bufio.NewReader(r)

	var written int
	var preImageOID, postImageOID []byte
	var haveOIDs, isBinary bool

	write := func(b []byte) error {
		n, err := w.Write(b)
		written += n
		return err
	}

	writeFragment := false
	continuation := false

	for {
		line, readErr := reader.ReadSlice('\n')

		// The final line of a patch may not be newline-terminated, in which case ReadSlice(3)
		// hands us the line and io.EOF at once.
		if len(line) > 0 && !continuation {
			writeFragment = false

			switch {
			case isBinary:
				// The remainder of a binary hunk is skipped up to the file header that
				// terminates it.
				if bytes.HasPrefix(line, []byte("diff ")) {
					isBinary = false
					haveOIDs = false
					// We hash this header even though git-patch-id(1) drops it. As the
					// whole point of this patch ID is collision resistance, we must not
					// inherit a collision that git leaves open: a file that directly
					// follows a binary diff without producing "index"/"---"/"+++" lines of
					// its own (e.g. a mode-only change) would otherwise contribute nothing
					// path-specific, so two patches that change the same binary but touch
					// different such files would hash the same. Hashing the header keeps
					// them distinct.
					writeFragment = true
				}
			case bytes.HasPrefix(line, []byte("diff ")):
				haveOIDs = false
				writeFragment = true
			case bytes.HasPrefix(line, []byte("index ")):
				// Blob IDs are deliberately left out: they change whenever the base version
				// of a file changes, even when the change itself has not. They are
				// remembered because a binary diff uses them in place of its content. The
				// copy is needed as the line is only valid until the next read.
				pre, post := parseIndexLine(line)
				if pre == nil {
					return 0, fmt.Errorf("malformed index line %q", line)
				}

				preImageOID = append(preImageOID[:0], pre...)
				postImageOID = append(postImageOID[:0], post...)
				haveOIDs = true
			case helper.ByteSliceHasAnyPrefix(line, "GIT binary patch", "Binary files"):
				// git-diff(1) is not asked for `--binary`, so the blob IDs are the only
				// thing that tells us a binary file changed at all. This is why the caller
				// must pass --full-index, which keeps them unabbreviated.
				if !haveOIDs {
					return 0, fmt.Errorf("binary diff without index line")
				}

				if err := write(preImageOID); err != nil {
					return 0, fmt.Errorf("writing normalized diff: %w", err)
				}
				if err := write(postImageOID); err != nil {
					return 0, fmt.Errorf("writing normalized diff: %w", err)
				}
				isBinary = true
			case bytes.HasPrefix(line, []byte("@@ -")):
				// Hunk headers are left out so that line offsets do not contribute to the
				// patch ID.
			default:
				writeFragment = true
			}
		}

		if len(line) > 0 && writeFragment {
			if err := write(line); err != nil {
				return 0, fmt.Errorf("writing normalized diff: %w", err)
			}
		}

		switch {
		case readErr == nil:
			continuation = false
		case errors.Is(readErr, bufio.ErrBufferFull):
			continuation = true
		case errors.Is(readErr, io.EOF):
			return written, nil
		default:
			return 0, fmt.Errorf("reading diff: %w", readErr)
		}
	}
}

// parseIndexLine extracts the pre- and post-image blob IDs from an "index <pre>..<post> <mode>"
// line. The mode is absent when the file was added or deleted.
func parseIndexLine(line []byte) ([]byte, []byte) {
	oids, ok := bytes.CutPrefix(line, []byte("index "))
	if !ok {
		return nil, nil
	}

	preImageOID, postImageOID, ok := bytes.Cut(oids, []byte(".."))
	if !ok {
		return nil, nil
	}

	if mode := bytes.IndexByte(postImageOID, ' '); mode >= 0 {
		postImageOID = postImageOID[:mode]
	}

	return preImageOID, bytes.TrimSuffix(postImageOID, []byte("\n"))
}

func validatePatchIDRequest(ctx context.Context, locator storage.Locator, in *gitalypb.GetPatchIDRequest) error {
	if err := locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return err
	}
	if string(in.GetOldRevision()) == "" {
		return errors.New("empty OldRevision")
	}
	if string(in.GetNewRevision()) == "" {
		return errors.New("empty NewRevision")
	}

	return nil
}
