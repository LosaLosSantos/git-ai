package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// This file builds the conflict package of a land that the server did not resolve (LandConflict,
// PLAN10 3.3): every conflicting file with its sides, its conflicting hunks and what the server did
// with it, so that the agent can resolve the conflict and land again with a LandResolution.

const (
	// landPackageMessageBytes bounds each commit message of the package.
	landPackageMessageBytes = 4 << 10
	// landPackageReasonBytes bounds every reason of the package.
	landPackageReasonBytes = 512
)

// Reasons of the files the server does not send to the resolver, besides the content rules of
// resolver.SendRules.
const (
	landNotSentTrailerUnsafe = "the path holds a line break or a NUL byte, which a commit trailer cannot carry"
	landNotSentInvalidUTF8   = "the path is not valid UTF-8, which the resolver protocol cannot carry"
	landNotSentDenied        = "the path may not be sent to the resolver (paths_allow, paths_deny)"
	landNotSentNotAFile      = "a side is not a file (a submodule or a directory)"
	landNotSentNoSide        = "the path is on neither side of the merge (a file/directory conflict)"
	landNotSentNoBaseOneSide = "the path has no base and a missing side, which no merge produces without a rename"
)

// landFileState is a conflicting file of a land and what the server did with it.
type landFileState struct {
	conflict landFileConflict
	kind     string
	// sides are the contents read for the resolver and the hunks (base, ours, theirs); nil for an
	// absent side or one that was not read.
	sides   [3][]byte
	read    bool
	outcome gitalypb.LandConflictFile_Outcome
	reason  string
}

// sent reports whether the file is sent to the resolver.
func (f *landFileState) sent() bool {
	return f.outcome == gitalypb.LandConflictFile_OUTCOME_UNSPECIFIED
}

// textRead reports whether the sides were read and are text, so that they have line hunks.
func (f *landFileState) textRead() bool {
	if !f.read {
		return false
	}
	for _, s := range f.sides {
		if bytes.IndexByte(s, 0) >= 0 {
			return false
		}
	}
	return true
}

// notSent marks the file as not sent, with its reason.
func (f *landFileState) notSent(reason string) {
	f.outcome = gitalypb.LandConflictFile_OUTCOME_NOT_SENT
	f.reason = reason
}

// landUnresolvedError is a land whose conflicts the server did not resolve. It carries the conflict
// package, which the caller attaches to the merge conflict error.
type landUnresolvedError struct {
	pkg   *gitalypb.LandConflict
	cause string
	// err is the error of the resolver or of the check, when one caused the outcome, so that
	// errors.Is and errors.As still see it.
	err error
}

func (e *landUnresolvedError) Error() string {
	return "conflicts not resolved: " + e.cause
}

// Unwrap returns the error of the resolver or of the check, if any.
func (e *landUnresolvedError) Unwrap() error {
	return e.err
}

// landWithPackage adds the conflict package of resolveErr, when it carries one, to the merge
// conflict error of the land as a second detail, after UserLandError. Clients that know only
// UserLandError see the error unchanged.
func landWithPackage(mergeConflict, resolveErr error) error {
	var unresolved *landUnresolvedError
	var se structerr.Error
	if !errors.As(resolveErr, &unresolved) || !errors.As(mergeConflict, &se) {
		return mergeConflict
	}
	return se.WithDetail(unresolved.pkg)
}

// landKindOf classifies a conflicting file from the presence of its sides.
func landKindOf(c landFileConflict) string {
	switch {
	case c.ours == nil || c.theirs == nil:
		return resolver.KindModifyDelete
	case c.base == nil:
		return resolver.KindAddAdd
	case c.ours.oid == c.theirs.oid:
		return resolver.KindMode
	default:
		return resolver.KindContent
	}
}

func landKindProto(kind string) gitalypb.LandConflictFile_Kind {
	switch kind {
	case resolver.KindContent:
		return gitalypb.LandConflictFile_KIND_CONTENT
	case resolver.KindModifyDelete:
		return gitalypb.LandConflictFile_KIND_MODIFY_DELETE
	case resolver.KindAddAdd:
		return gitalypb.LandConflictFile_KIND_ADD_ADD
	case resolver.KindMode:
		return gitalypb.LandConflictFile_KIND_MODE
	}
	return gitalypb.LandConflictFile_KIND_UNSPECIFIED
}

// landPackage returns the conflict package of the files, with reason set when the request was not
// attempted at all. At most maxFiles files are listed.
func landPackage(tip, commitID, base git.ObjectID, tipMessage, commitMessage string, files []*landFileState, maxFiles int, reason string) *gitalypb.LandConflict {
	tm, tipCut := landCut([]byte(tipMessage), landPackageMessageBytes)
	cm, commitCut := landCut([]byte(commitMessage), landPackageMessageBytes)
	pkg := &gitalypb.LandConflict{
		Tip:               tip.String(),
		Commit:            commitID.String(),
		MergeBase:         base.String(),
		TipMessage:        tm,
		CommitMessage:     cm,
		MessagesTruncated: tipCut || commitCut,
		Reason:            landCutString(reason, landPackageReasonBytes),
	}
	for i, f := range files {
		if i == maxFiles {
			break
		}
		pf := &gitalypb.LandConflictFile{
			Path:    []byte(f.conflict.path),
			Kind:    landKindProto(f.kind),
			Base:    landSideProto(f.conflict.base),
			Ours:    landSideProto(f.conflict.ours),
			Theirs:  landSideProto(f.conflict.theirs),
			Outcome: f.outcome,
			Reason:  landCutString(f.reason, landPackageReasonBytes),
		}
		if f.textRead() && (f.kind == resolver.KindContent || f.kind == resolver.KindAddAdd) {
			pf.Hunks = landConflictHunks(f.sides[0], f.sides[1], f.sides[2])
		}
		pkg.Files = append(pkg.Files, pf)
	}
	return pkg
}

func landSideProto(b *landBlob) *gitalypb.LandConflictSide {
	if b == nil {
		return nil
	}
	return &gitalypb.LandConflictSide{Oid: b.oid.String(), Mode: b.mode}
}

// landCut cuts b to at most n bytes on a UTF-8 boundary and reports whether it was cut.
func landCut(b []byte, n int) ([]byte, bool) {
	if len(b) <= n {
		return b, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return b[:cut], true
}

// landCutString cuts s to at most n bytes, valid UTF-8.
func landCutString(s string, n int) string {
	b, _ := landCut([]byte(s), n)
	return strings.ToValidUTF8(string(b), "")
}

// landGeneratedPaths returns the paths that the attributes of rev mark linguist-generated, with one
// git check-attr for all of them.
func landGeneratedPaths(ctx context.Context, repo *localrepo.Repo, rev git.ObjectID, paths []string) (map[string]bool, error) {
	generated := map[string]bool{}
	if len(paths) == 0 {
		return generated, nil
	}
	var stdin bytes.Buffer
	for _, p := range paths {
		stdin.WriteString(p)
		stdin.WriteByte(0)
	}
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "check-attr",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "-z"},
				gitcmd.Flag{Name: "--stdin"},
				gitcmd.ValueFlag{Name: "--source", Value: rev.String()},
			},
			Args: []string{"linguist-generated"},
		},
		gitcmd.WithStdin(&stdin),
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return nil, fmt.Errorf("check-attr: %w, stderr: %q", err, stderr.String())
	}
	// The output is NUL-separated triples: path, attribute, value.
	fields := strings.Split(strings.TrimSuffix(stdout.String(), "\x00"), "\x00")
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("check-attr: unexpected output of %d fields", len(fields))
	}
	for i := 0; i+2 < len(fields); i += 3 {
		switch fields[i+2] {
		case "set", "true":
			generated[fields[i]] = true
		}
	}
	return generated, nil
}
