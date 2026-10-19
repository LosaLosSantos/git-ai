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

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// This file implements the conflict resolution of UserLand (resolve protocol v1, see
// doc/graft/RESOLVE.md). When a landing hits a merge conflict and the request sets
// resolve_conflicts, the conflicting files are sent to the configured resolver (the client of
// internal/graft/resolver, which enforces the path and actor policy, the limits and every
// acceptance rule of the answer). The answer is written into a merge commit with parents
// [tip, commit]. Unless every file is resolved, the land fails with the original merge conflict
// and the conflict package of land_package.go.

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
// two. The result is a merge commit with parents [tip, commitID].
//
// Every conflicting file is decided on its own. A file that may not or cannot be sent (its path,
// its sides, or the never-send rules of resolver.SendRules) goes to the agent; the others go to
// the resolver in one request. Nothing lands partially: unless every file is resolved, the land
// fails with a *landUnresolvedError, whose conflict package says what happened to each file. A
// request the server does not attempt at all (resolution disabled, the actor not allowed, too many
// files, a criss-cross history) also gets a package, with the reason. Any other error means the
// conflicts could not even be listed, and the caller reports the merge conflict alone.
func (s *Server) resolveLandConflicts(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	tip, commitID git.ObjectID,
	committer git.Signature,
) (landResolution, error) {
	l, err := landListConflicts(ctx, repo, tip, commitID)
	if err != nil {
		return landResolution{}, err
	}
	base, hasBase, conflicts, files := l.base, l.hasBase, l.conflicts, l.files
	tipMessage, commitMessage := l.tipMessage, l.commitMessage
	maxFiles := s.resolveCfg.MaxFilesOrDefault()
	notAttempted := func(reason string) error { return l.notAttempted(maxFiles, reason, nil) }
	criss := l.bases

	// The actor is the authenticated identity only. The name in the request is chosen by the
	// caller, so it must not decide who may resolve: without an identity the actor is empty and
	// only an actor list that is empty (allows everyone) admits the request.
	actor := identity.Actor(ctx, "")
	switch {
	case s.conflictResolver == nil:
		return landResolution{}, notAttempted("conflict resolution is not enabled on this server")
	case !s.conflictResolver.ActorAllowed(actor):
		return landResolution{}, notAttempted(fmt.Sprintf("actor %q may not resolve conflicts", actor))
	case criss > 1:
		return landResolution{}, notAttempted(fmt.Sprintf("the change has %d merge bases (a criss-cross history), which are not resolved", criss))
	case len(conflicts) > maxFiles:
		return landResolution{}, notAttempted(fmt.Sprintf("%d conflicting files exceed max_files %d", len(conflicts), maxFiles))
	}

	if err := s.landClassify(ctx, repo, files, base, tip, commitID); err != nil {
		return landResolution{}, err
	}

	var sent []resolver.File
	for _, f := range files {
		if f.sent() {
			sent = append(sent, landRequestFile(f))
		}
	}

	var resp *resolver.Response
	var resolveErr error
	if len(sent) > 0 {
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
			Files:    sent,
		}
		if hasBase {
			req.MergeBase = base.String()
		}

		// The client applies the timeout, the request policy and the acceptance rules of the answer.
		resp, resolveErr = s.conflictResolver.Resolve(ctx, req)
		switch {
		case resolveErr == nil && !landResolverNameValid(resp.Resolver):
			// The name is written into the commit trailers, so it must be a plain single-line name.
			landFailSent(files, gitalypb.LandConflictFile_OUTCOME_FAILED, fmt.Sprintf("the resolver's answer names an invalid resolver %q", resp.Resolver))
			resp = nil
		case resolveErr != nil:
			landRefusalOutcomes(files, resolveErr)
		default:
			for _, f := range files {
				if f.sent() {
					f.outcome = gitalypb.LandConflictFile_OUTCOME_RESOLVED
				}
			}
		}
	}

	for _, f := range files {
		if f.outcome != gitalypb.LandConflictFile_OUTCOME_RESOLVED {
			return landResolution{}, l.unresolved(maxFiles, "", landUnresolvedCause(files), resolveErr)
		}
	}

	return s.landWriteResolution(ctx, in, repo, tip, commitID, l.mergedTree, committer, actor, resp, "")
}

// landListing is the conflicting files of the merge of a tip and a change, with what the conflict
// package needs.
type landListing struct {
	tip, commitID git.ObjectID
	base          git.ObjectID
	hasBase       bool
	// bases is the number of merge bases; more than one is a criss-cross history.
	bases                     int
	mergedTree                git.ObjectID
	conflicts                 []landFileConflict
	files                     []*landFileState
	tipMessage, commitMessage string
}

// errLandNoConflicts is the error of landListConflicts when the merge is clean.
var errLandNoConflicts = errors.New("the merge of the tip and the change reports no conflicting files")

// landListConflicts lists the files that conflict between tip and commitID. A clean merge is an
// error: when rebasing, the conflict may come from an intermediate commit that a later commit of
// the change undoes, so the merge of the two ends is clean and the landing reports the merge
// conflict. This is a documented limitation.
func landListConflicts(ctx context.Context, repo *localrepo.Repo, tip, commitID git.ObjectID) (landListing, error) {
	l := landListing{tip: tip, commitID: commitID}
	var err error
	if l.base, l.hasBase, l.bases, err = landMergeBase(ctx, repo, tip, commitID); err != nil {
		return l, err
	}
	if l.mergedTree, l.conflicts, err = landMergeTree(ctx, repo, tip, commitID, l.base, l.hasBase); err != nil {
		return l, err
	}
	if len(l.conflicts) == 0 {
		return l, errLandNoConflicts
	}
	if l.tipMessage, err = landCommitBody(ctx, repo, tip); err != nil {
		return l, err
	}
	if l.commitMessage, err = landCommitBody(ctx, repo, commitID); err != nil {
		return l, err
	}
	for _, c := range l.conflicts {
		l.files = append(l.files, &landFileState{conflict: c, kind: landKindOf(c)})
	}
	return l, nil
}

// unresolved returns the error of a land left unresolved, with its conflict package. err is the
// error of the resolver or of the check that caused it, if any.
func (l landListing) unresolved(maxFiles int, reason, cause string, err error) error {
	return &landUnresolvedError{
		pkg:   landPackage(l.tip, l.commitID, l.base, l.tipMessage, l.commitMessage, l.files, maxFiles, reason),
		cause: cause,
		err:   err,
	}
}

// notAttempted returns the error of a land the server did not try to resolve at all, every file
// NOT_ATTEMPTED and the package carrying the reason.
func (l landListing) notAttempted(maxFiles int, reason string, err error) error {
	for _, f := range l.files {
		f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, ""
	}
	return l.unresolved(maxFiles, reason, reason, err)
}

// landRequestFile is the file of the resolve request for a classified file that is sent.
func landRequestFile(f *landFileState) resolver.File {
	file := resolver.File{Path: f.conflict.path, Kind: f.kind}
	for i, side := range []struct {
		blob *landBlob
		dst  **resolver.Side
	}{{f.conflict.base, &file.Base}, {f.conflict.ours, &file.Ours}, {f.conflict.theirs, &file.Theirs}} {
		if side.blob != nil {
			*side.dst = &resolver.Side{OID: side.blob.oid.String(), Mode: side.blob.mode, Content: f.sides[i]}
		}
	}
	return file
}

// landFailSent sets the outcome and reason of every file that was sent.
func landFailSent(files []*landFileState, outcome gitalypb.LandConflictFile_Outcome, reason string) {
	for _, f := range files {
		if f.sent() {
			f.outcome, f.reason = outcome, reason
		}
	}
}

// landRefusalOutcomes sets the outcome of the sent files from the resolver's failure. A refusal
// with the outcome of every file (validated by the client) gives each its own; otherwise every
// sent file failed with the error.
func landRefusalOutcomes(files []*landFileState, err error) {
	var refusal *resolver.UnresolvableError
	if !errors.As(err, &refusal) || refusal.Files == nil {
		landFailSent(files, gitalypb.LandConflictFile_OUTCOME_FAILED, fmt.Sprintf("resolver failed: %v", err))
		return
	}
	outcomes := make(map[string]resolver.FileOutcome, len(refusal.Files))
	for _, o := range refusal.Files {
		outcomes[o.Path] = o
	}
	for _, f := range files {
		if !f.sent() {
			continue
		}
		o := outcomes[f.conflict.path]
		switch o.Outcome {
		case resolver.OutcomeResolved:
			f.outcome = gitalypb.LandConflictFile_OUTCOME_RESOLVED
		case resolver.OutcomeRejected:
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_REJECTED, o.Reason
		default:
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_FAILED, o.Reason
		}
	}
}

// landUnresolvedCause summarizes for the log why the files were not all resolved.
func landUnresolvedCause(files []*landFileState) string {
	var parts []string
	for _, f := range files {
		if f.outcome == gitalypb.LandConflictFile_OUTCOME_RESOLVED {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s %s", f.conflict.path, f.outcome, f.reason))
	}
	return strings.Join(parts, "; ")
}

// landWriteResolution writes a resolution (the resolver's answer, or the agent's) into the merged
// tree and commits it with parents [tip, commitID]; checkedBy names the resolver that checked an
// agent's resolution, if one did. The commit message carries the trailers.
func (s *Server) landWriteResolution(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	tip, commitID, mergedTree git.ObjectID,
	committer git.Signature,
	actor string,
	resp *resolver.Response,
	checkedBy string,
) (landResolution, error) {
	tree, err := repo.ReadTree(ctx, git.Revision(mergedTree.String()), localrepo.WithRecursive())
	if err != nil {
		return landResolution{}, fmt.Errorf("reading merged tree: %w", err)
	}
	paths := make([]string, 0, len(resp.Files))
	for _, f := range resp.Files {
		paths = append(paths, f.Path)
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
		Message:        landResolveCommitMessage(in, commitID, resp.Resolver, checkedBy, paths),
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
		"checked_by":   checkedBy,
		"paths":        paths,
		"explanation":  landTruncate(resp.Explanation, maxLoggedExplanationBytes),
	}).InfoContext(ctx, "graft: conflicts of land resolved")

	return landResolution{commit: newTip, resolvedPaths: resolvedPaths, resolver: resp.Resolver}, nil
}

// landClassify decides, for every file, whether it may be sent to the resolver, and reads the
// sides of those that may (and of those whose content decided against it, for the hunks of the
// package). The order: the path (a commit trailer must carry it, the protocol must carry it, the
// path policy must allow it and every path its sides come from), the shape of the sides, their
// size (from the object info, before reading), then the content rules of resolver.SendRules, with
// linguist-generated from the tip's attributes.
func (s *Server) landClassify(
	ctx context.Context,
	repo *localrepo.Repo,
	files []*landFileState,
	base, tip, commitID git.ObjectID,
) error {
	rules := s.conflictResolver.SendRules()
	listings := map[git.ObjectID]map[git.ObjectID][]string{}
	for _, f := range files {
		c := f.conflict
		switch {
		case !landTrailerSafe(c.path):
			f.notSent(landNotSentTrailerUnsafe)
			continue
		case !utf8.ValidString(c.path):
			f.notSent(landNotSentInvalidUTF8)
			continue
		case !s.conflictResolver.PathAllowed(c.path):
			f.notSent(landNotSentDenied)
			continue
		case landShapeReason(c) != "":
			f.notSent(landShapeReason(c))
			continue
		}
		// A rename makes a side come from another path than the name of the conflict, so the
		// policy is applied to the path of every side as well.
		denied, err := s.landDeniedSource(ctx, repo, listings, c, base, tip, commitID)
		if err != nil {
			return err
		}
		if denied != "" {
			f.notSent(denied)
			continue
		}

		tooLarge := false
		for _, b := range []*landBlob{c.base, c.ours, c.theirs} {
			if b == nil {
				continue
			}
			switch b.mode {
			case resolver.ModeRegular, resolver.ModeExecutable, resolver.ModeSymlink:
			default:
				f.notSent(landNotSentNotAFile)
			}
			if !f.sent() {
				break
			}
			info, err := repo.ReadObjectInfo(ctx, b.oid.Revision())
			if err != nil {
				return fmt.Errorf("reading size of %q: %w", c.path, err)
			}
			if rules.TooLarge(info.Size) {
				tooLarge = true
			}
		}
		if !f.sent() {
			continue
		}
		if tooLarge {
			f.notSent(resolver.NotSentLarge)
			continue
		}
		for i, b := range []*landBlob{c.base, c.ours, c.theirs} {
			if b == nil {
				continue
			}
			content, err := repo.ReadObject(ctx, b.oid)
			if err != nil {
				return fmt.Errorf("reading blob of %q: %w", c.path, err)
			}
			if content == nil {
				content = []byte{}
			}
			f.sides[i] = content
		}
		f.read = true
	}

	// The content rules, with linguist-generated read for every remaining path at once.
	var candidates []string
	for _, f := range files {
		if f.sent() {
			candidates = append(candidates, f.conflict.path)
		}
	}
	generated, err := landGeneratedPaths(ctx, repo, tip, candidates)
	if err != nil {
		return err
	}
	for _, f := range files {
		if !f.sent() {
			continue
		}
		if reason := rules.Reason(f.conflict.path, generated[f.conflict.path], f.sides[0], f.sides[1], f.sides[2]); reason != "" {
			f.notSent(reason)
		}
	}
	return nil
}

// landShapeReason returns why the shape of the sides keeps a conflict from the resolver, or "". A
// file/directory conflict has no version of the file on either side under its name. A conflict
// whose base is absent while one side is absent is not a conflict between two versions of a file,
// since no merge produces it without a rename; it cannot be answered reliably, so it fails closed.
func landShapeReason(c landFileConflict) string {
	switch {
	case c.ours == nil && c.theirs == nil:
		return landNotSentNoSide
	case c.base == nil && (c.ours == nil || c.theirs == nil):
		return landNotSentNoBaseOneSide
	}
	return ""
}

// landDeniedSource returns the reason when a side of the conflict comes from a path that may not
// be sent, and "" when every side's path may. merge-tree names a conflict after the path of the
// merged tree, but with a rename a side comes from another path: the base and one side may live
// under the old name. Without this check the content of a denied path could reach the resolver
// under an allowed name.
//
// A side lives at the name of the conflict when the tree of its commit holds the same blob there.
// Otherwise every path of that tree holding the blob is a possible source, and all of them must be
// allowed. A side whose blob is in no path of its tree is not sent, nor is a base when the merge
// has none to look it up in. Only trees are read, no blob.
func (s *Server) landDeniedSource(
	ctx context.Context,
	repo *localrepo.Repo,
	listings map[git.ObjectID]map[git.ObjectID][]string,
	c landFileConflict,
	base, tip, commitID git.ObjectID,
) (string, error) {
	for _, side := range []landSide{{c.base, base}, {c.ours, tip}, {c.theirs, commitID}} {
		if side.blob == nil {
			continue
		}
		if side.rev == "" {
			return "the path has a base side but the merge has no merge base to find it in", nil
		}
		sources, err := landSourcePaths(ctx, repo, listings, side.rev, side.blob.oid, c.path)
		if err != nil {
			return "", err
		}
		if len(sources) == 0 {
			return fmt.Sprintf("a side is in no path of %s", side.rev), nil
		}
		for _, p := range sources {
			if !landTrailerSafe(p) || !s.conflictResolver.PathAllowed(p) {
				return fmt.Sprintf("a side comes from path %q, which may not be sent to the resolver", p), nil
			}
		}
	}
	return "", nil
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

// landSourcePaths returns the paths of the tree of rev that the blob of a side of the conflict p
// may come from, none when the blob is in no path. listings caches the listing of every tree
// read, by commit.
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

	return listing[blob], nil
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
func landResolveCommitMessage(in *gitalypb.UserLandRequest, commitID git.ObjectID, resolverName, checkedBy string, paths []string) string {
	subject := strings.TrimRight(string(in.GetMergeMessage()), "\n")
	if subject == "" {
		subject = fmt.Sprintf("Merge %s into %s", landShortOID(commitID), string(in.GetBranch()))
	}

	var b strings.Builder
	b.WriteString(subject)
	b.WriteString("\n\n")
	b.WriteString("Graft-Resolved-By: " + resolverName + "\n")
	// An agent's resolution records whether the resolver's checks ran on it: without a resolver
	// it got only the marker rule and the server's structural validation.
	if checkedBy != "" {
		b.WriteString("Graft-Checked-By: " + checkedBy + "\n")
	}
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

// landMergeBase returns the merge base of a and b. Unrelated histories have none. A criss-cross
// history has several merge bases, whose number is returned: the base the resolver would be given
// would differ from the base of the recursive merge strategy, so such landings are not resolved,
// and hasBase is false.
func landMergeBase(ctx context.Context, repo *localrepo.Repo, a, b git.ObjectID) (base git.ObjectID, hasBase bool, bases int, err error) {
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{Name: "merge-base", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--all"}}, Args: []string{a.String(), b.String()}},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		if exitStatus, ok := command.ExitStatus(err); ok && exitStatus == 1 {
			return "", false, 0, nil
		}
		return "", false, 0, fmt.Errorf("merge-base: %w, stderr: %q", err, stderr.String())
	}

	all := strings.Fields(stdout.String())
	if len(all) != 1 {
		return "", false, len(all), nil
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", false, 0, fmt.Errorf("detecting object hash: %w", err)
	}
	base, err = objectHash.FromHex(all[0])
	if err != nil {
		return "", false, 0, fmt.Errorf("parsing merge base: %w", err)
	}
	return base, true, 1, nil
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
