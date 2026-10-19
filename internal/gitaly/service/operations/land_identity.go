package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// landIdentity is the earlier land of a change that is still on the branch (doc/graft/PLAN11.md,
// section 5): the record of the changelog that landed the change, and the commit that reverted it
// later, if one is found.
type landIdentity struct {
	// landed is the commit the earlier land put on the branch, which holds the change.
	landed git.ObjectID
	// sequence is the changelog sequence of the update of the earlier land.
	sequence uint64
	// revertedBy is the commit after landed on the branch that says it reverts landed, empty when
	// none is found.
	revertedBy git.ObjectID
}

// detail explains the answer, see UserLandResponse.already_landed_detail.
func (id *landIdentity) detail() string {
	detail := fmt.Sprintf("landed as %s at changelog %d", id.landed, id.sequence)
	if id.revertedBy != "" {
		detail += fmt.Sprintf("; it was reverted later by %s", id.revertedBy)
	}
	return detail
}

// setOn reports the earlier land on the response of an already landed request.
func (id *landIdentity) setOn(resp *gitalypb.UserLandResponse) {
	resp.LandedCommitId = id.landed.String()
	resp.LandedSequence = id.sequence
	resp.RevertedByCommitId = id.revertedBy.String()
	resp.AlreadyLandedDetail = id.detail()
}

// landWithRecord returns the binding that records the identities of lands on the branch reference
// in the changelog of the update performed with ctx (changelog.WithLands). The land must not run
// when it fails: it would land without its identity.
func landWithRecord(ctx context.Context, reference git.ReferenceName, lands []changelog.Land) (context.Context, error) {
	ctx, err := changelog.WithLands(ctx, reference.String(), lands)
	if err != nil {
		return ctx, structerr.NewInternal("recording the land identities: %w", err)
	}
	return ctx, nil
}

// landedEarlier returns the earlier land of commitID on the branch reference when it is still on
// the branch, that is when the commit it landed as is reachable from tip; nil otherwise. A change
// whose commit is not reachable from tip may have landed as a rewritten commit, and the record of
// that land is the only way to tell (doc/graft/PLAN11.md, section 5). Only then is the commit that
// reverted it looked for, to explain the answer.
func (a *landingAttempt) landedEarlier(ctx context.Context, reference git.ReferenceName, tip, commitID git.ObjectID) (*landIdentity, error) {
	record, found, err := a.server.lookupLand(ctx, a.repository, reference, commitID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	landed, err := a.objectHash.FromHex(record.Landed)
	if err != nil {
		return nil, structerr.NewInternal("land identity of %s: %w", commitID, err)
	}

	onBranch, err := a.repo.IsAncestor(ctx, landed.Revision(), tip.Revision())
	if err != nil {
		// The branch was reset past the earlier land and its commit was pruned since: the change
		// is no longer on the branch.
		if errors.As(err, new(localrepo.InvalidCommitError)) {
			return nil, nil
		}
		return nil, structerr.NewInternal("checking the earlier land of %s: %w", commitID, err)
	}
	if !onBranch {
		return nil, nil
	}

	// The revert only explains the answer, so a failure to look for it does not change the
	// answer: it is logged, and the answer names no revert.
	revertedBy, err := a.revertOf(ctx, landed, tip)
	if err != nil {
		a.server.logger.WithError(err).WithField("landed", landed.String()).
			Error("graft: looking for the revert of an earlier land failed; the answer names no revert")
	}

	return &landIdentity{landed: landed, sequence: record.Sequence, revertedBy: revertedBy}, nil
}

// revertSearchCommits bounds the commits that revertOf reads: the revert is looked for among the
// commits after the landed one that are closest to the tip, so that a long history after it does
// not slow the answer. Tests lower it.
var revertSearchCommits = 1000

// revertOf returns the latest commit after landed up to tip whose message holds "This reverts
// commit <landed>", the line that git revert and graft-verify write, or an empty ID when there is
// none among the revertSearchCommits commits closest to tip. A revert written another way, or
// further from the tip, is not found. The revert only explains the answer: the land is already
// landed whatever revertOf finds.
func (a *landingAttempt) revertOf(ctx context.Context, landed, tip git.ObjectID) (git.ObjectID, error) {
	var stdout, stderr bytes.Buffer
	if err := a.repo.ExecAndWait(ctx, gitcmd.Command{
		Name: "log",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "--format=%H%x00%B%x00"},
			gitcmd.Flag{Name: fmt.Sprintf("--max-count=%d", revertSearchCommits)},
		},
		Args: []string{landed.String() + ".." + tip.String()},
	}, gitcmd.WithStdout(&stdout), gitcmd.WithStderr(&stderr)); err != nil {
		return "", fmt.Errorf("looking for a revert of %s: %w, stderr: %q", landed, err, stderr.String())
	}

	// The output is pairs of fields, the commit ID and its message, each ended by a NUL, newest
	// commit first. Messages cannot hold a NUL.
	line := "This reverts commit " + landed.String()
	fields := strings.Split(stdout.String(), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		if !strings.Contains(fields[i+1], line) {
			continue
		}

		revertedBy, err := a.objectHash.FromHex(strings.TrimSpace(fields[i]))
		if err != nil {
			return "", fmt.Errorf("looking for a revert of %s: %w", landed, err)
		}
		return revertedBy, nil
	}

	return "", nil
}

// lookupLand returns the last land of original on the branch reference of repository that the
// changelog recorded, from where the changelog lives: the storage core's database in its
// authoritative mode, the file log of the repository otherwise. Without a changelog nothing is
// recorded, and nothing is found.
func (s *Server) lookupLand(ctx context.Context, repository *gitalypb.Repository, reference git.ReferenceName, original git.ObjectID) (changelog.LandRecord, bool, error) {
	if s.graftStorage.Authoritative() {
		repo, err := s.graftStorage.Meta.GetRepository(ctx, corestore.KeyOf(repository))
		if err != nil {
			// A repository that the database does not track has no recorded land. Whether it
			// can be written to is the write path's decision, as for any land.
			if errors.Is(err, corestore.ErrRepositoryNotFound) {
				return changelog.LandRecord{}, false, nil
			}
			return changelog.LandRecord{}, false, structerr.NewUnavailable("storage core: %w", err)
		}

		record, found, err := s.graftStorage.Meta.LookupLand(ctx, repo.ID, reference.String(), original.String())
		if err != nil {
			return changelog.LandRecord{}, false, structerr.NewUnavailable("storage core: %w", err)
		}
		return record, found, nil
	}

	if s.graft.Changelog.Disabled {
		return changelog.LandRecord{}, false, nil
	}

	repoPath, err := s.locator.GetRepoPath(ctx, repository)
	if err != nil {
		return changelog.LandRecord{}, false, err
	}

	record, found, err := changelog.LookupLand(repoPath, reference.String(), original.String())
	if err != nil {
		return changelog.LandRecord{}, false, structerr.NewInternal("looking up the land identity: %w", err)
	}
	return record, found, nil
}
