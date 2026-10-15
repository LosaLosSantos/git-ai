package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/feder-cr/open_scion/internal/command"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/graft/review/classify"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

const (
	// symlinkMode is the file mode of a symbolic link.
	symlinkMode = "120000"
	// gitlinkMode is the file mode of a submodule entry.
	gitlinkMode = "160000"
)

// diffEntry is one file changed between two trees, as printed by diff-tree.
type diffEntry struct {
	path    string
	oldMode string
	newMode string
	// oldID and newID are empty when the file was added or deleted.
	oldID git.ObjectID
	newID git.ObjectID
}

// parseDiffTree parses the output of `git diff-tree -r -z --no-renames`. Each
// record is a ":<old mode> <new mode> <old oid> <new oid> <status>" header and
// a path, both terminated by NUL.
func parseDiffTree(out []byte) ([]diffEntry, error) {
	fields := bytes.Split(out, []byte{0})
	// The output ends with a NUL, so the last field is empty.
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}

	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("diff-tree output has %d fields, want header and path pairs", len(fields))
	}

	entries := make([]diffEntry, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		header := string(fields[i])
		if !strings.HasPrefix(header, ":") {
			return nil, fmt.Errorf("malformed diff-tree header %q", header)
		}

		parts := strings.Fields(header[1:])
		if len(parts) != 5 {
			return nil, fmt.Errorf("malformed diff-tree header %q", header)
		}

		entry := diffEntry{
			path:    string(fields[i+1]),
			oldMode: parts[0],
			newMode: parts[1],
		}
		if !isNullOID(parts[2]) {
			entry.oldID = git.ObjectID(parts[2])
		}
		if !isNullOID(parts[3]) {
			entry.newID = git.ObjectID(parts[3])
		}

		if entry.path == "" {
			return nil, fmt.Errorf("diff-tree entry without path")
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// isNullOID reports whether oid is the all-zero object ID of either hash type.
func isNullOID(oid string) bool {
	return strings.Trim(oid, "0") == ""
}

// mergeBase returns the best common ancestor of a and b. The bool is false when
// they have none.
func mergeBase(ctx context.Context, repo *localrepo.Repo, a, b git.ObjectID) (git.ObjectID, bool, error) {
	var stdout bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "merge-base",
			Args: []string{a.String(), b.String()},
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(io.Discard),
	); err != nil {
		if code, ok := command.ExitStatus(err); ok && code == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("merge-base: %w", err)
	}

	return git.ObjectID(strings.TrimSpace(stdout.String())), true, nil
}

// isMergeBase reports whether candidate is one of the best common ancestors of
// a and b (`git merge-base --all`). Commits without a common ancestor have no
// merge base.
func isMergeBase(ctx context.Context, repo *localrepo.Repo, candidate, a, b git.ObjectID) (bool, error) {
	var stdout bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "merge-base",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--all"}},
			Args:  []string{a.String(), b.String()},
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(io.Discard),
	); err != nil {
		if code, ok := command.ExitStatus(err); ok && code == 1 {
			return false, nil
		}
		return false, fmt.Errorf("merge-base --all: %w", err)
	}

	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.TrimSpace(line) == candidate.String() {
			return true, nil
		}
	}

	return false, nil
}

// resolveCommit resolves rev to the ID of a commit. A missing revision is
// NotFound, a revision naming another kind of object is InvalidArgument.
func resolveCommit(ctx context.Context, repo *localrepo.Repo, rev, field string) (git.ObjectID, error) {
	if rev == "" {
		return "", structerr.NewInvalidArgument("%s is required", field)
	}
	if strings.HasPrefix(rev, "-") {
		return "", structerr.NewInvalidArgument("%s must not start with a dash", field)
	}

	oid, err := repo.ResolveRevision(ctx, git.Revision(rev))
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return "", structerr.NewNotFound("%s: revision %q not found", field, rev)
		}
		return "", structerr.NewInternal("resolve %s: %w", field, err)
	}

	info, err := repo.ReadObjectInfo(ctx, oid.Revision())
	if err != nil {
		if errors.As(err, new(localrepo.InvalidObjectError)) {
			return "", structerr.NewNotFound("%s: object %q not found", field, rev)
		}
		return "", structerr.NewInternal("read %s: %w", field, err)
	}
	if info.Type != "commit" {
		return "", structerr.NewInvalidArgument("%s must be a commit, got %s", field, info.Type)
	}

	return oid, nil
}

// resolveBranch returns the tip of a local branch.
func resolveBranch(ctx context.Context, repo *localrepo.Repo, name string) (git.ObjectID, error) {
	oid, err := repo.ResolveRevision(ctx, git.Revision("refs/heads/"+name))
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return "", structerr.NewNotFound("branch %q not found", name)
		}
		return "", structerr.NewInternal("resolve branch %q: %w", name, err)
	}

	return oid, nil
}

// validBranchName reports whether name can name a branch under refs/heads.
// Names that git would read as revision syntax are refused.
func validBranchName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.Contains(name, "..") {
		return false
	}
	if strings.ContainsAny(name, " ~^:?*[\\") {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// changedFiles lists the files changed between from and to, classified.
func changedFiles(ctx context.Context, repo *localrepo.Repo, from, to git.ObjectID) ([]*gitalypb.ReviewFile, error) {
	var stdout bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "diff-tree",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "-r"},
				gitcmd.Flag{Name: "-z"},
				gitcmd.Flag{Name: "--no-renames"},
			},
			Args: []string{from.String(), to.String()},
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(io.Discard),
	); err != nil {
		return nil, fmt.Errorf("diff-tree: %w", err)
	}

	entries, err := parseDiffTree(stdout.Bytes())
	if err != nil {
		return nil, err
	}

	files := make([]*gitalypb.ReviewFile, 0, len(entries))
	for _, entry := range entries {
		class, reason, err := classifyEntry(ctx, repo, entry)
		if err != nil {
			return nil, err
		}
		files = append(files, &gitalypb.ReviewFile{
			Path:   []byte(entry.path),
			Class:  class,
			Reason: reason,
		})
	}

	return files, nil
}

// maxClassifiedBlobBytes caps the size of a blob whose content is read for
// classification. A larger blob is not read at all and is classified as CODE.
const maxClassifiedBlobBytes = 1 << 20

// classifyEntry classifies one changed file. The file mode is part of the
// change, so a change that alters the mode, turns a file into a symlink or
// touches a submodule is never classified by its content alone: the content
// of a symlink or submodule does not show what the change does, and a mode
// change with an identical blob is not a comment or docs change.
func classifyEntry(ctx context.Context, repo *localrepo.Repo, entry diffEntry) (gitalypb.ReviewFile_ChangeClass, string, error) {
	if reason, ok := unclassifiableMode(entry); ok {
		return gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, reason, nil
	}

	oldContent, oldTooLarge, err := sideContent(ctx, repo, entry.oldID)
	if err != nil {
		return gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, "", err
	}
	newContent, newTooLarge, err := sideContent(ctx, repo, entry.newID)
	if err != nil {
		return gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED, "", err
	}
	if oldTooLarge || newTooLarge {
		return gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED,
			fmt.Sprintf("a version is larger than %d bytes and is not classified", maxClassifiedBlobBytes), nil
	}

	class, reason := classify.File(entry.path, oldContent, newContent)
	return class, reason, nil
}

// unclassifiableMode reports whether the modes of a change make a content
// classification unsafe, and why.
func unclassifiableMode(entry diffEntry) (string, bool) {
	bothSides := entry.oldID != "" && entry.newID != ""
	if bothSides && entry.oldMode != entry.newMode {
		return fmt.Sprintf("file mode changed from %s to %s", entry.oldMode, entry.newMode), true
	}
	for _, mode := range []string{entry.oldMode, entry.newMode} {
		switch mode {
		case symlinkMode:
			return "symlink: the content is a link target, not the code it resolves to", true
		case gitlinkMode:
			return "submodule: the content is a commit ID, not the code it points to", true
		}
	}
	return "", false
}

// sideContent returns the content one side of a change is classified on. A
// missing side is nil. The bool is true when the blob is larger than
// maxClassifiedBlobBytes, in which case its content is not read.
func sideContent(ctx context.Context, repo *localrepo.Repo, oid git.ObjectID) ([]byte, bool, error) {
	if oid == "" {
		return nil, false, nil
	}

	info, err := repo.ReadObjectInfo(ctx, oid.Revision())
	if err != nil {
		if errors.As(err, new(localrepo.InvalidObjectError)) {
			// A blob that is not in the object database cannot be classified.
			// Treat it like a missing side rather than failing the review.
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read object info %s: %w", oid, err)
	}
	if info.Size > maxClassifiedBlobBytes {
		return nil, true, nil
	}

	content, err := repo.ReadObject(ctx, oid)
	if err != nil {
		if errors.As(err, new(localrepo.InvalidObjectError)) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read blob %s: %w", oid, err)
	}

	return content, false, nil
}
