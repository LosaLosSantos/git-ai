package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// A resolution that a model made is never landed by the server: it is a proposal, which the agent
// reviews, edits or replaces, and lands as its own (PLAN10 14). The server keeps the proposals of a
// failed land under a hidden reference, which the agent reads by object ID (GetObjects).

// landProposalPruneErrors counts the prunings of proposal references that failed after a land.
var landProposalPruneErrors = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_land_proposal_prune_errors_total",
	Help: "Prunings of the proposal references of a branch that failed after a land.",
})

// landProposalWriteErrors counts the answers that could not be kept as proposals: the land still
// fails with its package, whose files say "proposal not stored".
var landProposalWriteErrors = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_land_proposal_write_errors_total",
	Help: "Resolver answers of a failed land that could not be kept as proposals.",
})

// landProposedReason is the reason of a file that a model resolved.
const landProposedReason = "a model resolved the file: the server never lands a model's resolution, it proposes it (see proposal); land it, edited or not, as your own resolution"

// landAnswerFlagsProposals reports whether a file of the answer is marked as a proposal.
func landAnswerFlagsProposals(answer *resolver.Response) bool {
	for _, f := range answer.Files {
		if f.Proposal {
			return true
		}
	}
	return false
}

// landAnswerProposes reports whether a resolver's answer holds a model's resolution: a file marked
// as a proposal, or, should a resolver omit the mark, a resolver that names the llm strategy.
func landAnswerProposes(answer *resolver.Response) bool {
	if landAnswerFlagsProposals(answer) {
		return true
	}
	name := answer.Resolver
	if i := strings.IndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	for _, strategy := range strings.Split(name, ",") {
		if strings.TrimSpace(strategy) == "llm" {
			return true
		}
	}
	return false
}

// landWriteProposals keeps the resolver's answer for the agent: the written contents as blobs, a
// commit whose tree holds them by path (its parents the tip and the landed commit), and the
// proposal reference of (branch, commit) pointing at it, replacing an older one. The objects are
// written into the repository itself, not the quarantine of the land, which a failed land discards,
// and the reference is updated through the reference transaction (and so the voter in
// authoritative storage mode). It sets the proposal of every file of the listing and returns the
// reference. The branch's older proposals are pruned after; a failed pruning is logged and
// counted, and fails nothing.
func (s *Server) landWriteProposals(ctx context.Context, in *gitalypb.UserLandRequest, l landListing, answer *resolver.Response, committer git.Signature) (git.ReferenceName, error) {
	// Who caused the proposals is in their commit, the trace of the reference: the update itself
	// is the server's (landUpdateProposalRefs).
	actor := landBatchActor(ctx, in)
	// The date of the proposals' commit is the server's clock, never the request's timestamp:
	// their age decides when they expire.
	now := time.Now()
	start := now
	repo := s.localRepoFactory.Build(in.GetRepository())
	tree := &localrepo.TreeEntry{Type: localrepo.Tree, Mode: "040000"}
	byPath := make(map[string]*gitalypb.LandConflictProposal, len(answer.Files))
	explanation := landCutString(answer.Explanation, landPackageReasonBytes)
	for _, f := range answer.Files {
		p := &gitalypb.LandConflictProposal{Action: f.Action, Resolver: answer.Resolver, Explanation: explanation}
		if f.Action == resolver.ActionWrite {
			blob, err := repo.WriteBlob(ctx, bytes.NewReader(f.Content), localrepo.WriteBlobConfig{Path: f.Path})
			if err != nil {
				return "", fmt.Errorf("writing the proposal of %q: %w", f.Path, err)
			}
			entry := localrepo.TreeEntry{OID: blob, Mode: f.Mode, Path: path.Base(f.Path), Type: localrepo.Blob}
			if err := tree.Add(f.Path, entry, localrepo.WithOverwriteFile()); err != nil {
				return "", fmt.Errorf("adding the proposal of %q: %w", f.Path, err)
			}
			p.Oid, p.Mode = blob.String(), f.Mode
		}
		byPath[f.Path] = p
	}
	if err := tree.Write(ctx, repo); err != nil {
		return "", fmt.Errorf("writing the tree of the proposals: %w", err)
	}
	commit, err := repo.WriteCommit(ctx, localrepo.WriteCommitConfig{
		TreeID:         tree.OID,
		Message:        fmt.Sprintf("graft: proposals for landing %s onto %s\n\nResolver: %s\nRequested-By: %s\n", l.commitID, l.tip, landTrailerValueOrQuoted(answer.Resolver), landTrailerValueOrQuoted(actor)),
		Parents:        []git.ObjectID{l.tip, l.commitID},
		AuthorName:     committer.Name,
		AuthorEmail:    committer.Email,
		AuthorDate:     now,
		CommitterName:  committer.Name,
		CommitterEmail: committer.Email,
		CommitterDate:  now,
		GitConfig:      s.gitConfig,
	})
	if err != nil {
		return "", fmt.Errorf("writing the commit of the proposals: %w", err)
	}

	ref := proposals.Ref(string(in.GetBranch()), l.commitID)
	old, err := repo.GetReference(ctx, ref)
	oldOID := git.ObjectID("")
	switch {
	case err == nil:
		oldOID = git.ObjectID(old.Target)
	case !errors.Is(err, git.ErrReferenceNotFound):
		return "", fmt.Errorf("reading %s: %w", ref, err)
	}
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", fmt.Errorf("detecting object hash: %w", err)
	}
	if oldOID == "" {
		oldOID = objectHash.ZeroOID
	}
	if err := proposals.Update(ctx, repo, []proposals.Op{{Name: ref, NewOID: commit, OldOID: oldOID}}); err != nil {
		return "", err
	}
	s.logger.WithFields(log.Fields{"ref": ref.String(), "actor": actor, "resolver": answer.Resolver, "seconds": time.Since(start).Seconds()}).InfoContext(ctx, "graft: kept the resolver's answer as proposals")

	for _, f := range l.files {
		f.proposal = byPath[f.conflict.path]
	}
	if err := s.landPruneProposals(ctx, repo, string(in.GetBranch()), ""); err != nil {
		landProposalPruneErrors.Inc()
		s.logger.WithError(err).WithField("branch", string(in.GetBranch())).ErrorContext(ctx, "graft: pruning the proposals of the branch failed")
	}
	return ref, nil
}

// landPruneAfterLand prunes the proposals of the branch after a land that asked for resolution or
// brought one, and drops the proposal of the change that landed. Only those lands prune, so that a
// plain land pays nothing for proposals; the bound of landMaxProposalsPerBranch holds anyway, since
// only those lands write proposals. The land has happened: a failure is logged and counted, and
// the land's result stands.
func (s *Server) landPruneAfterLand(ctx context.Context, in *gitalypb.UserLandRequest) {
	if !in.GetResolveConflicts() && in.GetResolution() == nil {
		return
	}
	repo := s.localRepoFactory.Build(in.GetRepository())
	drop := git.ReferenceName("")
	if commitID, err := repo.ResolveRevision(ctx, git.Revision(in.GetCommitId()+"^{commit}")); err == nil {
		drop = proposals.Ref(string(in.GetBranch()), commitID)
	}
	if err := s.landPruneProposals(ctx, repo, string(in.GetBranch()), drop); err != nil {
		landProposalPruneErrors.Inc()
		s.logger.WithError(err).WithField("branch", string(in.GetBranch())).ErrorContext(ctx, "graft: pruning the proposals of the branch failed after the land")
	}
}

// landPruneProposals prunes the proposals of a branch (proposals.Prune), with drop when set.
func (s *Server) landPruneProposals(ctx context.Context, repo *localrepo.Repo, branch string, drop git.ReferenceName) error {
	ttl, err := s.resolveCfg.ProposalTTLOrDefault()
	if err != nil {
		return err
	}
	_, err = proposals.Prune(ctx, repo, proposals.BranchDir(branch), ttl, drop, time.Now())
	return err
}

// landTrailerValueOrQuoted returns v when it is a valid trailer value (landTrailerValueValid), and
// v quoted otherwise, so that a value the caller chose (the user ID of the request) can never add
// a line to the proposals' message.
func landTrailerValueOrQuoted(v string) string {
	if landTrailerValueValid(v) {
		return v
	}
	return strconv.QuoteToASCII(v)
}

// decideLand is the one decision point of a land that asked the server to resolve its conflicts
// (PLAN10 15.4): it lands only when the resolver answered, the answer holds no model's resolution,
// and every conflicting file is resolved. A file that proposes is never landed. This is where a
// landing gate for model resolutions would live; today there is none.
func decideLand(answer *resolver.Response, files []*landFileState) bool {
	if answer == nil {
		return false
	}
	if landAnswerProposes(answer) {
		return false
	}
	for _, f := range files {
		if f.outcome != gitalypb.LandConflictFile_OUTCOME_RESOLVED {
			return false
		}
	}
	return true
}

// landProposalsNotStored marks every file of the answer: its proposal was not stored, and why.
func landProposalsNotStored(files []*landFileState, answer *resolver.Response, cause error) {
	inAnswer := make(map[string]bool, len(answer.Files))
	for _, f := range answer.Files {
		inAnswer[f.Path] = true
	}
	for _, f := range files {
		if !inAnswer[f.conflict.path] {
			continue
		}
		f.proposal = nil
		note := "proposal not stored: " + cause.Error()
		if f.reason == "" {
			f.reason = note
		} else {
			f.reason += "; " + note
		}
	}
}

// landPolicyDeniedError is the policy's refusal of a land that asked the server to resolve: the
// agent may not update the branch, so nothing is resolved and no proposal is kept. err is the
// refusal (PermissionDenied).
type landPolicyDeniedError struct{ err error }

func (e *landPolicyDeniedError) Error() string { return e.err.Error() }

func (e *landPolicyDeniedError) Unwrap() error { return e.err }
