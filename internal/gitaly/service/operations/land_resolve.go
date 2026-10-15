package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc/status"

	"github.com/feder-cr/open_scion/internal/command"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/graft/identity"
	"github.com/feder-cr/open_scion/internal/graft/resolver"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// This file implements the conflict resolution of UserLand (resolve protocol v1, see
// doc/graft/RESOLVE.md). When a landing hits a merge conflict and the request sets
// resolve_conflicts, the conflicting files are sent to the configured resolver (the client of
// internal/graft/resolver, which enforces the path and actor policy, the limits and every
// acceptance rule of the answer). The answer is written into a merge commit with parents
// [tip, commit]. Any failure leaves the original merge conflict in place.

// maxResolverNameBytes bounds the name of the resolver, which is written into a commit trailer.
const maxResolverNameBytes = resolver.MaxResolverNameBytes

// maxLoggedExplanationBytes bounds the explanation of the resolver kept in the log of a
// resolution.
const maxLoggedExplanationBytes = 4 << 10

// landResolution is the merge commit that resolves a landing.
type landResolution struct {
	commit        git.ObjectID
	resolvedPaths [][]byte
	resolver      string
}

// landBlob is one side of a conflicted file as merge-tree reports it: the mode and the blob.
type landBlob struct {
	mode string
	oid  git.ObjectID
}

// landFileConflict is a conflicted file of the merge. The sides are the stage entries of merge-tree,
// so they are the versions of the file that the merge actually compared. A rename changes the
// path of a side, which the stage entries still follow: the name of the conflict is the path of
// the merged tree, and the base may have lived under another name. A nil side is absent.
type landFileConflict struct {
	path   string
	base   *landBlob
	ours   *landBlob
	theirs *landBlob
}

// resolveLandConflicts lands commitID onto tip by resolving the files that conflict between the
// two. The result is a merge commit with parents [tip, commitID]. An error means the conflict
// could not be resolved, and the caller reports the original merge conflict.
//
// The policy is checked before any blob is read or the resolver is called: the actor, the number
// of conflicting files and every path must be allowed.
func (s *Server) resolveLandConflicts(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	tip, commitID git.ObjectID,
	committer git.Signature,
) (landResolution, error) {
	if s.conflictResolver == nil {
		return landResolution{}, errors.New("conflict resolution is not enabled")
	}

	// The actor is the authenticated identity only. The name in the request is chosen by the
	// caller, so it must not decide who may resolve: without an identity the actor is empty and
	// only an actor list that is empty (allows everyone) admits the request.
	actor := identity.Actor(ctx, "")
	if !s.conflictResolver.ActorAllowed(actor) {
		return landResolution{}, fmt.Errorf("actor %q may not resolve conflicts", actor)
	}

	base, hasBase, err := landMergeBase(ctx, repo, tip, commitID)
	if err != nil {
		return landResolution{}, err
	}

	mergedTree, conflicts, err := landMergeTree(ctx, repo, tip, commitID, base, hasBase)
	if err != nil {
		return landResolution{}, err
	}
	// A clean merge tree is not resolved here. When rebasing, the conflict may come from an
	// intermediate commit that a later commit of the change undoes, so the merge of the two ends
	// is clean and the landing reports the merge conflict. This is a documented limitation.
	if len(conflicts) == 0 {
		return landResolution{}, errors.New("the merge of the tip and the change reports no conflicting files")
	}
	if len(conflicts) > s.resolveCfg.MaxFilesOrDefault() {
		return landResolution{}, fmt.Errorf("%d conflicting files exceed max_files", len(conflicts))
	}
	paths := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		paths = append(paths, c.path)
	}
	for _, p := range paths {
		// The paths end up in commit trailers, where a line break would inject trailers.
		if !landTrailerSafe(p) {
			return landResolution{}, fmt.Errorf("conflicting path %q cannot be resolved", p)
		}
		// Git paths are bytes, but the protocol carries them as JSON strings, which hold
		// Unicode text only: the resolver would see another name and its answer could never
		// match. Such conflicts are not resolved.
		if !utf8.ValidString(p) {
			return landResolution{}, fmt.Errorf("conflicting path %q is not valid UTF-8 and cannot be sent to the resolver", p)
		}
		if !s.conflictResolver.PathAllowed(p) {
			return landResolution{}, fmt.Errorf("path %q may not be sent to the resolver", p)
		}
	}
	// A rename makes a side of a conflict come from another path than the name of the conflict,
	// so the policy is applied to the path of every side as well.
	if err := s.landCheckSourcePaths(ctx, repo, conflicts, base, tip, commitID); err != nil {
		return landResolution{}, err
	}

	maxBytes := s.resolveCfg.MaxFileBytesOrDefault()
	files, err := landResolveFiles(ctx, repo, conflicts, maxBytes)
	if err != nil {
		return landResolution{}, err
	}

	tipMessage, err := landCommitBody(ctx, repo, tip)
	if err != nil {
		return landResolution{}, err
	}
	commitMessage, err := landCommitBody(ctx, repo, commitID)
	if err != nil {
		return landResolution{}, err
	}

	req := resolver.Request{
		Version: resolver.ProtocolVersion,
		Repository: resolver.Repository{
			Storage:      in.GetRepository().GetStorageName(),
			RelativePath: in.GetRepository().GetRelativePath(),
		},
		Branch:   string(in.GetBranch()),
		Tip:      tip.String(),
		Commit:   commitID.String(),
		Messages: resolver.Messages{Tip: tipMessage, Commit: commitMessage},
		Actor:    actor,
		Files:    files,
		Limits:   resolver.Limits{MaxFileBytes: maxBytes},
	}
	if hasBase {
		req.MergeBase = base.String()
	}

	// The client applies the timeout, the request policy and the acceptance rules of the answer.
	resp, err := s.conflictResolver.Resolve(ctx, req)
	if err != nil {
		return landResolution{}, fmt.Errorf("resolver failed: %w", err)
	}

	// The name is written into the commit trailers, so it must be a plain single-line name.
	if !landResolverNameValid(resp.Resolver) {
		return landResolution{}, fmt.Errorf("resolver answer names an invalid resolver %q", resp.Resolver)
	}

	tree, err := repo.ReadTree(ctx, git.Revision(mergedTree.String()), localrepo.WithRecursive())
	if err != nil {
		return landResolution{}, fmt.Errorf("reading merged tree: %w", err)
	}
	for _, f := range resp.Files {
		if f.Action == resolver.ActionDelete {
			if err := tree.Delete(f.Path); err != nil && !errors.Is(err, localrepo.ErrEntryNotFound) {
				return landResolution{}, fmt.Errorf("deleting %q: %w", f.Path, err)
			}
			continue
		}

		blob, err := repo.WriteBlob(ctx, bytes.NewReader(f.Content), localrepo.WriteBlobConfig{Path: f.Path})
		if err != nil {
			return landResolution{}, fmt.Errorf("writing resolved %q: %w", f.Path, err)
		}
		entry := localrepo.TreeEntry{OID: blob, Mode: f.Mode, Path: path.Base(f.Path), Type: localrepo.Blob}
		if err := tree.Add(f.Path, entry, localrepo.WithOverwriteFile()); err != nil {
			return landResolution{}, fmt.Errorf("adding resolved %q: %w", f.Path, err)
		}
	}
	if err := tree.Write(ctx, repo); err != nil {
		return landResolution{}, fmt.Errorf("writing merged tree: %w", err)
	}

	newTip, err := repo.WriteCommit(ctx, localrepo.WriteCommitConfig{
		TreeID:         tree.OID,
		Message:        landResolveCommitMessage(in, commitID, resp.Resolver, paths),
		Parents:        []git.ObjectID{tip, commitID},
		AuthorName:     committer.Name,
		AuthorEmail:    committer.Email,
		AuthorDate:     committer.When,
		CommitterName:  committer.Name,
		CommitterEmail: committer.Email,
		CommitterDate:  committer.When,
		GitConfig:      s.gitConfig,
	})
	if err != nil {
		return landResolution{}, fmt.Errorf("writing merge commit: %w", err)
	}

	resolvedPaths := make([][]byte, 0, len(paths))
	for _, p := range paths {
		resolvedPaths = append(resolvedPaths, []byte(p))
	}

	// The server keeps its own record of every resolution and of what the resolver said about
	// it, next to the trailers of the commit. The branch update may still fail or be retried, so
	// this records the resolution and not the landing.
	s.logger.WithFields(log.Fields{
		"branch":       string(in.GetBranch()),
		"tip":          tip.String(),
		"commit":       commitID.String(),
		"merge_commit": newTip.String(),
		"actor":        actor,
		"resolver":     resp.Resolver,
		"paths":        paths,
		"explanation":  landTruncate(resp.Explanation, maxLoggedExplanationBytes),
	}).InfoContext(ctx, "graft: conflicts of land resolved")

	return landResolution{commit: newTip, resolvedPaths: resolvedPaths, resolver: resp.Resolver}, nil
}

// landTrailerSafe reports whether s can be written as a single line of a commit trailer: it must
// hold no line break and no NUL byte.
func landTrailerSafe(s string) bool {
	return !strings.ContainsAny(s, "\r\n\x00")
}

// landTruncate returns s cut to at most n bytes, with a marker when it was cut. The result is
// valid UTF-8: a rune cut in half and any invalid byte are dropped.
func landTruncate(s string, n int) string {
	if len(s) <= n {
		return strings.ToValidUTF8(s, "")
	}
	return strings.ToValidUTF8(s[:n], "") + " [truncated]"
}

// landResolverNameValid reports whether name can be the name of the resolver, which the answer
// chooses and which is written into a commit trailer and the response. It must be non-empty, at
// most maxResolverNameBytes long, valid UTF-8 and printable: control characters such as tab or
// escape sequences, and Unicode line or paragraph separators, are rejected.
func landResolverNameValid(name string) bool {
	if name == "" || len(name) > maxResolverNameBytes || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// landSide is one present side of a conflicting file and the commit whose tree it comes from.
type landSide struct {
	blob *landBlob
	rev  git.ObjectID
}

// landCheckSourcePaths applies the path policy to the path every side of every conflict comes
// from. merge-tree names a conflict after the path of the merged tree, but with a rename a side
// comes from another path: the base and one side may live under the old name. Without this check
// the content of a denied path could reach the resolver under an allowed name.
//
// A side lives at the name of the conflict when the tree of its commit holds the same blob there.
// Otherwise every path of that tree holding the blob is a possible source, and all of them must
// be allowed. A side whose blob is in no path of its tree is refused, as is any side when there is
// no merge base to look it up in. Only trees are read, no blob.
func (s *Server) landCheckSourcePaths(
	ctx context.Context,
	repo *localrepo.Repo,
	conflicts []landFileConflict,
	base, tip, commitID git.ObjectID,
) error {
	listings := map[git.ObjectID]map[git.ObjectID][]string{}
	for _, c := range conflicts {
		for _, side := range []landSide{{c.base, base}, {c.ours, tip}, {c.theirs, commitID}} {
			if side.blob == nil {
				continue
			}
			if side.rev == "" {
				return fmt.Errorf("conflicting path %q has a base but the merge has none", c.path)
			}

			sources, err := landSourcePaths(ctx, repo, listings, side.rev, side.blob.oid, c.path)
			if err != nil {
				return err
			}
			for _, p := range sources {
				if !landTrailerSafe(p) || !s.conflictResolver.PathAllowed(p) {
					return fmt.Errorf("conflicting path %q comes from path %q, which may not be sent to the resolver", c.path, p)
				}
			}
		}
	}
	return nil
}

// landSourcePaths returns the paths of the tree of rev that the blob of a side of the conflict p
// may come from. listings caches the listing of every tree read, by commit.
func landSourcePaths(
	ctx context.Context,
	repo *localrepo.Repo,
	listings map[git.ObjectID]map[git.ObjectID][]string,
	rev, blob git.ObjectID,
	p string,
) ([]string, error) {
	// The common case: the side was not renamed and lives at the name of the conflict.
	info, err := repo.ReadObjectInfo(ctx, git.Revision(rev.String()+":"+p))
	if err == nil && info.Oid == blob {
		return []string{p}, nil
	}
	var notFound localrepo.InvalidObjectError
	if err != nil && !errors.As(err, &notFound) {
		return nil, fmt.Errorf("looking up %q in %s: %w", p, rev, err)
	}

	listing, ok := listings[rev]
	if !ok {
		if listing, err = landListTree(ctx, repo, rev); err != nil {
			return nil, err
		}
		listings[rev] = listing
	}

	sources := listing[blob]
	if len(sources) == 0 {
		return nil, fmt.Errorf("a side of conflicting path %q is in no path of %s", p, rev)
	}
	return sources, nil
}

// landListTree lists the tree of rev recursively and returns the paths of every object it holds.
func landListTree(ctx context.Context, repo *localrepo.Repo, rev git.ObjectID) (map[git.ObjectID][]string, error) {
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "ls-tree",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "-r"},
				gitcmd.Flag{Name: "-z"},
				gitcmd.Flag{Name: "--full-tree"},
			},
			Args: []string{rev.String()},
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return nil, fmt.Errorf("ls-tree %s: %w, stderr: %q", rev, err, stderr.String())
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	paths := map[git.ObjectID][]string{}
	for _, record := range strings.Split(stdout.String(), "\x00") {
		if record == "" {
			continue
		}
		// "<mode> SP <type> SP <oid> TAB <path>"
		meta, p, ok := strings.Cut(record, "\t")
		parts := strings.Fields(meta)
		if !ok || len(parts) != 3 {
			return nil, fmt.Errorf("unexpected ls-tree record %q", record)
		}
		oid, err := objectHash.FromHex(parts[2])
		if err != nil {
			return nil, fmt.Errorf("parsing object ID of %q: %w", p, err)
		}
		paths[oid] = append(paths[oid], p)
	}
	return paths, nil
}

// landResolveCommitMessage returns the message of the resolution commit: the merge message or
// the default one, followed by the Graft-Resolved trailers.
func landResolveCommitMessage(in *gitalypb.UserLandRequest, commitID git.ObjectID, resolverName string, paths []string) string {
	subject := strings.TrimRight(string(in.GetMergeMessage()), "\n")
	if subject == "" {
		subject = fmt.Sprintf("Merge %s into %s", landShortOID(commitID), string(in.GetBranch()))
	}

	var b strings.Builder
	b.WriteString(subject)
	b.WriteString("\n\n")
	b.WriteString("Graft-Resolved-By: " + resolverName + "\n")
	for _, p := range paths {
		b.WriteString("Graft-Resolved-Path: " + p + "\n")
	}
	return b.String()
}

// landShortOID returns the abbreviated object ID used in default messages.
func landShortOID(oid git.ObjectID) string {
	const short = 7
	s := oid.String()
	if len(s) > short {
		return s[:short]
	}
	return s
}

// landCommitBody returns the message of a commit.
func landCommitBody(ctx context.Context, repo *localrepo.Repo, commit git.ObjectID) (string, error) {
	c, err := repo.ReadCommit(ctx, commit.Revision())
	if err != nil {
		return "", fmt.Errorf("reading commit message of %s: %w", commit, err)
	}
	return string(c.GetBody()), nil
}

// landMergeBase returns the merge base of a and b. Unrelated histories have none. Criss-cross
// histories have several merge bases, and the base the resolver is given would then differ from
// the base of the recursive merge strategy. Such landings are not resolved.
func landMergeBase(ctx context.Context, repo *localrepo.Repo, a, b git.ObjectID) (git.ObjectID, bool, error) {
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{Name: "merge-base", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--all"}}, Args: []string{a.String(), b.String()}},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		if exitStatus, ok := command.ExitStatus(err); ok && exitStatus == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("merge-base: %w, stderr: %q", err, stderr.String())
	}

	bases := strings.Fields(stdout.String())
	if len(bases) == 0 {
		return "", false, nil
	}
	if len(bases) > 1 {
		return "", false, fmt.Errorf("the change has %d merge bases (criss-cross history), which are not resolved", len(bases))
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", false, fmt.Errorf("detecting object hash: %w", err)
	}
	base, err := objectHash.FromHex(bases[0])
	if err != nil {
		return "", false, fmt.Errorf("parsing merge base: %w", err)
	}
	return base, true, nil
}

// landMergeTree runs merge-tree of tip and commitID. It returns the tree of the merge, which holds
// the conflicting files in their conflicted state, and the conflicting files with their stage
// entries.
//
// Without --name-only, merge-tree prints a record per stage of every conflicting file, as
// "<mode> SP <oid> SP <stage> TAB <path>", where stage 1 is the base, 2 ours and 3 theirs. The
// sides are taken from these records and not by reading the path again on each side, because a
// rename makes the path of a side differ from the name of the conflict.
func landMergeTree(
	ctx context.Context,
	repo *localrepo.Repo,
	tip, commitID, base git.ObjectID,
	hasBase bool,
) (git.ObjectID, []landFileConflict, error) {
	flags := []gitcmd.Option{
		gitcmd.Flag{Name: "-z"},
		gitcmd.Flag{Name: "--write-tree"},
		gitcmd.Flag{Name: "--allow-unrelated-histories"},
	}
	if hasBase {
		flags = append(flags, gitcmd.ValueFlag{Name: "--merge-base", Value: base.String()})
	}

	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{Name: "merge-tree", Flags: flags, Args: []string{tip.String(), commitID.String()}},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		// Exit status 1 means the merge has conflicts, and the tree is printed regardless.
		if exitStatus, ok := command.ExitStatus(err); !ok || exitStatus != 1 {
			return "", nil, fmt.Errorf("merge-tree: %w, stderr: %q", err, stderr.String())
		}
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("detecting object hash: %w", err)
	}

	// The output is the tree OID, the conflicted file records and, after an empty field, the
	// informational messages.
	fields := strings.Split(stdout.String(), "\x00")
	treeOID, err := objectHash.FromHex(fields[0])
	if err != nil {
		return "", nil, fmt.Errorf("parsing merge tree: %w", err)
	}

	var conflicts []landFileConflict
	index := map[string]int{}
	for _, record := range fields[1:] {
		if record == "" {
			break
		}

		meta, p, ok := strings.Cut(record, "\t")
		parts := strings.Fields(meta)
		if !ok || p == "" || len(parts) != 3 {
			return "", nil, fmt.Errorf("unexpected conflicted file record %q", record)
		}

		oid, err := objectHash.FromHex(parts[1])
		if err != nil {
			return "", nil, fmt.Errorf("parsing blob ID of %q: %w", p, err)
		}

		i, found := index[p]
		if !found {
			i = len(conflicts)
			index[p] = i
			conflicts = append(conflicts, landFileConflict{path: p})
		}
		// The index is used and not a pointer, since appending may move the slice.
		c := &conflicts[i]

		var side **landBlob
		switch parts[2] {
		case "1":
			side = &c.base
		case "2":
			side = &c.ours
		case "3":
			side = &c.theirs
		default:
			return "", nil, fmt.Errorf("unexpected stage %q of conflicted path %q", parts[2], p)
		}
		if *side != nil {
			return "", nil, fmt.Errorf("conflicted path %q has two entries of stage %s", p, parts[2])
		}
		*side = &landBlob{mode: parts[0], oid: oid}
	}

	return treeOID, conflicts, nil
}

// landResolveFiles returns the base, ours and theirs versions of the conflicting files.
func landResolveFiles(
	ctx context.Context,
	repo *localrepo.Repo,
	conflicts []landFileConflict,
	maxBytes int64,
) ([]resolver.File, error) {
	files := make([]resolver.File, 0, len(conflicts))
	for _, c := range conflicts {
		f := resolver.File{Path: c.path}

		var err error
		if f.Base, err = landReadSide(ctx, repo, c.base, c.path, maxBytes); err != nil {
			return nil, err
		}
		if f.Ours, err = landReadSide(ctx, repo, c.ours, c.path, maxBytes); err != nil {
			return nil, err
		}
		if f.Theirs, err = landReadSide(ctx, repo, c.theirs, c.path, maxBytes); err != nil {
			return nil, err
		}

		if err := landCheckSides(f); err != nil {
			return nil, err
		}

		f.Kind = landConflictKind(f)
		files = append(files, f)
	}
	return files, nil
}

// landCheckSides rejects the shapes of the sides that the conflict cannot have for a file that
// both ends can answer. A file/directory conflict has no version of the file on one side under
// its name, so no side is present and the file is not sent. A conflict whose base is absent while
// one side is absent is not a conflict between two versions of a file, since no merge produces it
// without a rename. It cannot be answered reliably, so it fails closed.
func landCheckSides(f resolver.File) error {
	if f.Ours == nil && f.Theirs == nil {
		return fmt.Errorf("conflicting path %q is on neither side of the merge, which is a file/directory conflict", f.Path)
	}
	if f.Base == nil && (f.Ours == nil || f.Theirs == nil) {
		return fmt.Errorf("conflicting path %q has no base and a missing side, which is not resolved", f.Path)
	}
	return nil
}

// landReadSide reads the version of a conflicting file from its stage entry. It returns nil if the
// side is absent. Sides that are not files, or larger than maxBytes, are an error because they
// cannot be sent.
func landReadSide(ctx context.Context, repo *localrepo.Repo, blob *landBlob, p string, maxBytes int64) (*resolver.Side, error) {
	if blob == nil {
		return nil, nil
	}

	switch blob.mode {
	case resolver.ModeRegular, resolver.ModeExecutable, resolver.ModeSymlink:
	default:
		return nil, fmt.Errorf("path %q is not a file on one side of the conflict", p)
	}

	info, err := repo.ReadObjectInfo(ctx, blob.oid.Revision())
	if err != nil {
		return nil, fmt.Errorf("reading size of %q: %w", p, err)
	}
	if info.Size > maxBytes {
		return nil, fmt.Errorf("file %q is %d bytes, larger than max_file_bytes", p, info.Size)
	}

	content, err := repo.ReadObject(ctx, blob.oid)
	if err != nil {
		return nil, fmt.Errorf("reading blob of %q: %w", p, err)
	}

	return &resolver.Side{OID: blob.oid.String(), Mode: blob.mode, Content: content}, nil
}

// landConflictKind classifies a conflicting file from the presence of its versions.
func landConflictKind(f resolver.File) string {
	switch {
	case f.Ours == nil || f.Theirs == nil:
		return resolver.KindModifyDelete
	case f.Base == nil:
		return resolver.KindAddAdd
	case f.Ours.OID == f.Theirs.OID:
		return resolver.KindMode
	default:
		return resolver.KindContent
	}
}

// isLandMergeConflict reports whether a landing failed because the change does not apply cleanly
// onto the tip.
func isLandMergeConflict(err error) bool {
	for _, detail := range status.Convert(err).Details() {
		if landErr, ok := detail.(*gitalypb.UserLandError); ok && landErr.GetMergeConflict() != nil {
			return true
		}
	}
	return false
}
