package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// A resolution that a model made is never landed by the server: it is a proposal, which the agent
// reviews, edits or replaces, and lands as its own (PLAN10 14). The server keeps the proposals of a
// failed land under a hidden reference, which the agent reads by object ID (GetObjects).

const (
	// landProposalRefPrefix is the hidden namespace of the proposals (git.InternalRefPrefixes).
	landProposalRefPrefix = "refs/graft/proposals/"
	// landMaxProposalsPerBranch bounds the proposal references kept for one branch.
	landMaxProposalsPerBranch = 20
)

// landProposalPruneErrors counts the proposal references that could not be pruned after a land.
var landProposalPruneErrors = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_land_proposal_prune_errors_total",
	Help: "Prunings of the proposal references of a branch that failed after a land.",
})

// landProposalBranchDir is the directory of the proposal references of a branch: a hash of the
// branch name, so that any branch name gives a valid and fixed-length component.
func landProposalBranchDir(branch string) string {
	sum := sha256.Sum256([]byte(branch))
	return landProposalRefPrefix + hex.EncodeToString(sum[:])[:16] + "/"
}

// landProposalRef is the reference that keeps the proposals of the land of commit onto branch.
func landProposalRef(branch string, commit git.ObjectID) git.ReferenceName {
	return git.ReferenceName(landProposalBranchDir(branch) + commit.String())
}

// landProposalsError is the failure to keep a resolver's answer as proposals. The land fails with
// Internal (landWithPackage): a package without the proposals would hide the answer.
type landProposalsError struct{ err error }

func (e *landProposalsError) Error() string {
	return "keeping the resolver's answer as proposals: " + e.err.Error()
}

func (e *landProposalsError) Unwrap() error { return e.err }

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
		Message:        fmt.Sprintf("graft: proposals for landing %s onto %s\n\nResolver: %s\nRequested-By: %s\n", l.commitID, l.tip, answer.Resolver, actor),
		Parents:        []git.ObjectID{l.tip, l.commitID},
		AuthorName:     committer.Name,
		AuthorEmail:    committer.Email,
		AuthorDate:     committer.When,
		CommitterName:  committer.Name,
		CommitterEmail: committer.Email,
		CommitterDate:  committer.When,
		GitConfig:      s.gitConfig,
	})
	if err != nil {
		return "", fmt.Errorf("writing the commit of the proposals: %w", err)
	}

	ref := landProposalRef(string(in.GetBranch()), l.commitID)
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
	if err := s.landUpdateProposalRefs(ctx, repo, []landProposalRefOp{{name: ref, newOID: commit, oldOID: oldOID}}); err != nil {
		return "", err
	}
	s.logger.WithFields(log.Fields{"ref": ref.String(), "actor": actor, "resolver": answer.Resolver}).InfoContext(ctx, "graft: kept the resolver's answer as proposals")

	for _, f := range l.files {
		f.proposal = byPath[f.conflict.path]
	}
	if err := s.landPruneProposals(ctx, repo, string(in.GetBranch()), ""); err != nil {
		landProposalPruneErrors.Inc()
		s.logger.WithError(err).WithField("branch", string(in.GetBranch())).ErrorContext(ctx, "graft: pruning the proposals of the branch failed")
	}
	return ref, nil
}

// landPruneProposals deletes the proposal references of a branch that are older than proposal_ttl
// (by the committer date of their commit) or beyond the newest landMaxProposalsPerBranch, and drop,
// when set: the proposal of a commit that landed. The deletions go through the reference
// transaction, as the writes do.
func (s *Server) landPruneProposals(ctx context.Context, repo *localrepo.Repo, branch string, drop git.ReferenceName) error {
	ttl, err := s.resolveCfg.ProposalTTLOrDefault()
	if err != nil {
		return err
	}
	refs, err := repo.GetReferences(ctx, landProposalBranchDir(branch))
	if err != nil {
		return fmt.Errorf("listing the proposals of %q: %w", branch, err)
	}
	type dated struct {
		name git.ReferenceName
		when time.Time
	}
	var kept []dated
	var remove []git.ReferenceName
	now := time.Now()
	for _, r := range refs {
		if r.Name == drop {
			remove = append(remove, r.Name)
			continue
		}
		c, err := repo.ReadCommit(ctx, git.Revision(r.Target))
		if err != nil {
			return fmt.Errorf("reading %s: %w", r.Name, err)
		}
		when := c.GetCommitter().GetDate().AsTime()
		if now.Sub(when) > ttl {
			remove = append(remove, r.Name)
			continue
		}
		kept = append(kept, dated{r.Name, when})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].when.After(kept[j].when) })
	for i := landMaxProposalsPerBranch; i < len(kept); i++ {
		remove = append(remove, kept[i].name)
	}
	if len(remove) == 0 {
		return nil
	}
	deletions := make([]landProposalRefOp, 0, len(remove))
	for _, name := range remove {
		deletions = append(deletions, landProposalRefOp{name: name, delete: true})
	}
	if err := s.landUpdateProposalRefs(ctx, repo, deletions); err != nil {
		return fmt.Errorf("pruning proposals: %w", err)
	}
	return nil
}

// landProposalRefOp is one update of a proposal reference: to newOID from oldOID, or a deletion.
type landProposalRefOp struct {
	name           git.ReferenceName
	newOID, oldOID git.ObjectID
	delete         bool
}

// landUpdateProposalRefs is the one writer of the proposal references (PLAN10 14): it updates
// only references of the form refs/graft/proposals/<16 hex of the branch>/<commit ID>, and refuses
// anything else before git runs. It updates them as the server, without the identity of the
// request, through the reference transaction like any update: the agent whose land made the
// proposals may not write refs/graft/ itself (the reference-transaction hook reserves it), and
// its policy does not judge the server's write. Who caused it is in the proposals' commit.
func (s *Server) landUpdateProposalRefs(ctx context.Context, repo *localrepo.Repo, ops []landProposalRefOp) error {
	for _, op := range ops {
		if !landProposalRefNameValid(op.name) {
			return fmt.Errorf("refusing to update %q: not a proposal reference", op.name)
		}
	}
	ctx = identity.NewContext(ctx, identity.Identity{})
	updater, err := updateref.New(ctx, repo)
	if err != nil {
		return fmt.Errorf("updating proposal references: %w", err)
	}
	defer func() { _ = updater.Close() }()
	if err := updater.Start(); err != nil {
		return fmt.Errorf("updating proposal references: %w", err)
	}
	for _, op := range ops {
		if op.delete {
			err = updater.Delete(op.name)
		} else {
			err = updater.Update(op.name, op.newOID, op.oldOID)
		}
		if err != nil {
			return fmt.Errorf("updating %s: %w", op.name, err)
		}
	}
	if err := updater.Commit(); err != nil {
		return fmt.Errorf("updating proposal references: %w", err)
	}
	return nil
}

// landProposalRefNameValid reports whether name is refs/graft/proposals/<16 hex>/<object ID>.
func landProposalRefNameValid(name git.ReferenceName) bool {
	rest, ok := strings.CutPrefix(name.String(), landProposalRefPrefix)
	if !ok {
		return false
	}
	dir, commit, ok := strings.Cut(rest, "/")
	return ok && len(dir) == 16 && landHex(dir) && (len(commit) == 40 || len(commit) == 64) && landHex(commit)
}

// landHex reports whether s is lower-case hexadecimal.
func landHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
		drop = landProposalRef(string(in.GetBranch()), commitID)
	}
	if err := s.landPruneProposals(ctx, repo, string(in.GetBranch()), drop); err != nil {
		landProposalPruneErrors.Inc()
		s.logger.WithError(err).WithField("branch", string(in.GetBranch())).ErrorContext(ctx, "graft: pruning the proposals of the branch failed after the land")
	}
}
