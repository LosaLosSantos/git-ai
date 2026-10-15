package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/feder-cr/open_scion/internal/command"
	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/git/mvcc"
	"github.com/feder-cr/open_scion/internal/git/quarantine"
	gitupdateref "github.com/feder-cr/open_scion/internal/git/updateref"
	"github.com/feder-cr/open_scion/internal/gitaly/hook"
	"github.com/feder-cr/open_scion/internal/gitaly/hook/updateref"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/storagemgr/partition/conflict/refdb"
	"github.com/feder-cr/open_scion/internal/graft/ancestry"
	"github.com/feder-cr/open_scion/internal/graft/changelog"
	"github.com/feder-cr/open_scion/internal/graft/derived"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// defaultLandMaxAttempts is the number of compare-and-swap attempts UserLand makes when the
// request does not set max_attempts.
const defaultLandMaxAttempts = 20

const (
	// landBackoffBase is the backoff ceiling after the first lost race. The ceiling doubles with
	// every further attempt up to landBackoffMax.
	landBackoffBase = 2 * time.Millisecond
	// landBackoffMax caps the backoff ceiling.
	landBackoffMax = 100 * time.Millisecond
)

// landConflict reports that the compare-and-swap of the branch lost a race against a concurrent
// writer. The attempt is discarded and UserLand retries from the top.
type landConflict struct {
	reference      git.ReferenceName
	oldOID, newOID git.ObjectID
	cause          error
}

// Error returns the error message.
func (e landConflict) Error() string {
	return fmt.Sprintf("reference %s moved concurrently: %v", e.reference, e.cause)
}

// Unwrap returns the underlying reference update error.
func (e landConflict) Unwrap() error {
	return e.cause
}

// UserLand lands commit_id onto a branch. See the proto documentation of the RPC for the
// semantics. Every attempt works in its own quarantine directory: the tip is read, the new
// history is computed and the branch is updated with a compare-and-swap against the tip that
// was read. If the branch moved in the meantime, the attempt is retried.
//
// On MVCC repositories, the attempt runs against a snapshot that is pinned for the whole RPC.
// Retrying within the RPC would read the same tip again, so the conflict is reported to the
// client instead and the client has to retry the landing.
func (s *Server) UserLand(ctx context.Context, in *gitalypb.UserLandRequest) (*gitalypb.UserLandResponse, error) {
	if err := validateUserLandRequest(ctx, s.locator, in); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	committer, err := git.SignatureFromRequest(in)
	if err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	if s.landBatchable(ctx, in) {
		return s.userLandBatched(ctx, in, committer)
	}

	resp, err := s.landWithRetries(ctx, in, committer)
	if err != nil {
		return nil, err
	}

	// A request that lands alone is a batch of one.
	resp.BatchSize = 1
	resp.BatchPosition = 1

	return resp, nil
}

// landWithRetries lands the request alone, retrying the compare-and-swap while the branch moves. It
// makes at most max_attempts attempts.
//
// On MVCC repositories, the attempt runs against a snapshot that is pinned for the whole RPC.
// Retrying within the RPC would read the same tip again, so the conflict is reported to the
// client instead and the client has to retry the landing.
func (s *Server) landWithRetries(ctx context.Context, in *gitalypb.UserLandRequest, committer git.Signature) (*gitalypb.UserLandResponse, error) {
	snapshotPinned := mvcc.CacheFromContext(ctx) != nil

	maxAttempts := int(in.GetMaxAttempts())
	if maxAttempts == 0 {
		maxAttempts = defaultLandMaxAttempts
	}

	for attempt := 1; ; attempt++ {
		resp, err := s.landAttempt(ctx, in, committer)
		if err == nil {
			resp.Attempts = int32(attempt)
			return resp, nil
		}

		var conflict landConflict
		if !errors.As(err, &conflict) {
			return nil, err
		}

		if attempt >= maxAttempts || snapshotPinned {
			return nil, landKeptMovingError(in, conflict, attempt)
		}

		if err := waitLandBackoff(ctx, attempt); err != nil {
			return nil, structerr.NewCanceled("waiting to retry landing: %w", err)
		}

		// In authoritative storage mode the branch may have moved on another node: the next
		// attempt must read the tip that the database has (see doc/graft/STORAGE.md).
		if err := s.ensureStorageFresh(ctx, in.GetRepository()); err != nil {
			return nil, err
		}
	}
}

// landAncestry returns the ancestry checker of one landing attempt. The graph index is used only
// when [graft.land] graph_ancestry is set; otherwise the checker runs git. The index is stored in
// the derived-data store unless [graft.derived] disabled is set. The returned function
// releases the object reader of the checker and must be called once the attempt is over.
func (s *Server) landAncestry(ctx context.Context, quarantineRepo *localrepo.Repo) (ancestry.Checker, func(), error) {
	if !s.graft.Land.GraphAncestry {
		return ancestry.New(ctx, false, quarantineRepo, nil, nil, s.logger), func() {}, nil
	}

	// The quarantine repository reads the objects of the request, which the landed commit may be
	// one of.
	reader, release, err := s.catfileCache.ObjectReader(ctx, quarantineRepo)
	if err != nil {
		return nil, nil, structerr.NewInternal("reading objects for ancestry: %w", err)
	}

	// The quarantine shares the path of the repository, so the stored entries are those of the
	// repository itself.
	repoPath, err := quarantineRepo.Path(ctx)
	if err != nil {
		release()
		return nil, nil, structerr.NewInternal("getting repository path for ancestry: %w", err)
	}

	// With [graft.derived] disabled = true nothing is written to the derived-data store: the
	// graph entries are computed in memory for this attempt only.
	var store *derived.Store
	if !s.graft.Derived.Disabled {
		store = derived.NewStore(repoPath)
	}

	checker := ancestry.New(ctx, true, quarantineRepo, reader, store, s.logger)
	return checker, release, nil
}

// landAttempt performs a single compare-and-swap attempt of UserLand. It returns a landConflict
// if the branch was moved by a concurrent writer, in which case the caller retries.
func (s *Server) landAttempt(ctx context.Context, in *gitalypb.UserLandRequest, committer git.Signature) (*gitalypb.UserLandResponse, error) {
	quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, in.GetRepository())
	if err != nil {
		return nil, err
	}
	defer cleanup()

	checker, releaseChecker, err := s.landAncestry(ctx, quarantineRepo)
	if err != nil {
		return nil, err
	}
	defer releaseChecker()

	objectHash, err := quarantineRepo.ObjectHash(ctx)
	if err != nil {
		return nil, structerr.NewInternal("detecting object hash: %w", err)
	}

	commitID, err := objectHash.FromHex(in.GetCommitId())
	if err != nil {
		return nil, structerr.NewInvalidArgument("cannot parse commit ID: %w", err).
			WithMetadata("commit_id", in.GetCommitId())
	}

	// The ID may refer to a tag object or another non-commit object that peels to a commit. A
	// branch can only point to a commit, so the peeled commit is the one that lands.
	commitID, err = quarantineRepo.ResolveRevision(ctx, git.Revision(commitID.String()+"^{commit}"))
	if err != nil {
		return nil, structerr.NewInvalidArgument("cannot resolve commit ID: %w", err).
			WithMetadata("commit_id", in.GetCommitId())
	}

	reference := git.NewReferenceNameFromBranchName(string(in.GetBranch()))

	tip, err := quarantineRepo.ResolveRevision(ctx, reference.Revision())
	if err != nil {
		if !errors.Is(err, git.ErrReferenceNotFound) {
			return nil, structerr.NewInternal("resolving branch: %w", err)
		}

		if !in.GetCreateBranch() {
			return nil, structerr.NewNotFound("branch %q does not exist", string(in.GetBranch()))
		}

		// The branch does not exist yet and may be created at the commit. The
		// compare-and-swap expects the zero OID, so a concurrent creation is detected.
		if err := s.updateLandedReference(ctx, in, quarantineDir, quarantineRepo, reference, objectHash.ZeroOID, commitID, false); err != nil {
			return nil, err
		}

		return landResponse(objectHash.ZeroOID, commitID, false, false, true), nil
	}

	// The commit is already reachable from the tip, so there is nothing to do. This also covers
	// commit_id being equal to the tip.
	landed, err := checker.IsAncestor(ctx, commitID, tip)
	if err != nil {
		return nil, structerr.NewInternal("checking whether commit is already landed: %w", err)
	}
	if landed {
		return landResponse(tip, tip, false, true, false), nil
	}

	newTip := commitID
	rewritten := false
	var resolution *landResolution

	fastForward, err := checker.IsAncestor(ctx, tip, commitID)
	if err != nil {
		return nil, structerr.NewInternal("checking for ancestry: %w", err)
	}

	if !fastForward {
		newTip, err = s.computeLandedTip(ctx, in, quarantineRepo, tip, commitID, committer)
		if err != nil {
			if !in.GetResolveConflicts() || !isLandMergeConflict(err) {
				return nil, err
			}

			// The conflict is resolved into a merge commit. When that fails, the client gets
			// the merge conflict it would have got without resolution.
			res, resolveErr := s.resolveLandConflicts(ctx, in, quarantineRepo, tip, commitID, committer)
			if resolveErr != nil {
				s.logger.WithError(resolveErr).WithField("branch", string(in.GetBranch())).
					Info("graft: conflict resolution of land failed")
				return nil, err
			}

			resolution = &res
			newTip = res.commit
		}
		rewritten = true

		// Rebasing drops the commits that become empty on top of the tip, so every commit
		// of the change was dropped when the tip already holds all of its changes, for
		// example because they were applied upstream as cherry-picks. commit_id itself is
		// not reachable from the tip, so the change is not reported as already landed: the
		// response reports a landing that left the branch unchanged.
		if resolution == nil && newTip == tip {
			return landResponse(tip, tip, false, false, false), nil
		}
	}

	if err := s.updateLandedReference(ctx, in, quarantineDir, quarantineRepo, reference, tip, newTip, rewritten); err != nil {
		return nil, err
	}

	resp := landResponse(tip, newTip, rewritten, false, false)
	if resolution != nil {
		resp.ResolvedPaths = resolution.resolvedPaths
		resp.Resolver = resolution.resolver
	}
	return resp, nil
}

// computeLandedTip computes the new branch tip for a commit that is neither reachable from the
// tip nor a fast-forward of it. It rebases or merges the change onto the tip, depending on the
// strategy. FF_ONLY fails with a NotAncestor error.
func (s *Server) computeLandedTip(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	quarantineRepo *localrepo.Repo,
	tip, commitID git.ObjectID,
	committer git.Signature,
) (git.ObjectID, error) {
	switch in.GetStrategy() {
	case gitalypb.UserLandRequest_STRATEGY_FF_ONLY:
		return "", structerr.NewFailedPrecondition("branch %q cannot be fast-forwarded to commit", string(in.GetBranch())).
			WithDetail(&gitalypb.UserLandError{
				Error: &gitalypb.UserLandError_NotAncestor{
					NotAncestor: &gitalypb.NotAncestorError{
						ParentRevision: []byte(tip.String()),
						ChildRevision:  []byte(commitID.String()),
					},
				},
			})

	case gitalypb.UserLandRequest_STRATEGY_MERGE:
		mergeMessage := string(in.GetMergeMessage())
		if mergeMessage == "" {
			mergeMessage = fmt.Sprintf("Merge commit '%s' into %s", commitID, string(in.GetBranch()))
		}

		mergeCommitID, err := s.merge(ctx, quarantineRepo,
			committer,
			committer,
			mergeMessage,
			tip.String(),
			commitID.String(),
			false,
			false,
		)
		if err != nil {
			var conflictErr *localrepo.MergeTreeConflictError
			if errors.As(err, &conflictErr) {
				return "", landMergeConflictError(tip, commitID, conflictingFileNames(conflictErr), conflictErr)
			}

			return "", structerr.NewInternal("merging commit: %w", err)
		}

		return git.ObjectID(mergeCommitID), nil

	case gitalypb.UserLandRequest_STRATEGY_REBASE_UNSPECIFIED:
		if err := rejectMergeCommitsInRange(ctx, quarantineRepo, tip, commitID); err != nil {
			return "", err
		}

		// Rebasing needs a merge base. Unrelated histories can only be landed with STRATEGY_MERGE.
		related, err := hasMergeBase(ctx, quarantineRepo, tip, commitID)
		if err != nil {
			return "", structerr.NewInternal("checking for a common ancestor: %w", err)
		}
		if !related {
			return "", structerr.NewInvalidArgument("commit shares no history with branch %q: use STRATEGY_MERGE to land unrelated history", string(in.GetBranch()))
		}

		// Commits that are patch-equivalent to a commit of the tip's history are reapplied
		// instead of being dropped. The tip may have reverted such a commit since, and
		// dropping it would lose its changes while the landing succeeds. A reapplied commit
		// whose changes the tip still holds becomes empty and is dropped by the rebase. One
		// whose changes the tip modified further conflicts, which is reported rather than
		// guessed at.
		rebasedCommitID, err := quarantineRepo.Rebase(
			ctx,
			tip.String(),
			commitID.String(),
			localrepo.RebaseWithCommitter(committer),
			localrepo.RebaseWithReappliedCherryPicks(),
		)
		if err != nil {
			// The conflict error is passed as the cause itself, not the structerr-wrapped
			// error from Rebase, because structerr takes the code of the innermost error.
			var conflictErr *localrepo.RebaseConflictError
			if errors.As(err, &conflictErr) {
				return "", landMergeConflictError(tip, commitID, conflictingFileNames(conflictErr.ConflictError), conflictErr)
			}

			return "", structerr.NewInternal("rebasing commit: %w", err)
		}

		return rebasedCommitID, nil

	default:
		return "", structerr.NewInvalidArgument("unknown strategy: %d", in.GetStrategy())
	}
}

// rejectMergeCommitsInRange fails when the commits reachable from commit but not from tip contain
// a merge commit. Rebasing drops merge commits, so such a change can only be landed with
// STRATEGY_MERGE.
func rejectMergeCommitsInRange(ctx context.Context, repo *localrepo.Repo, tip, commitID git.ObjectID) error {
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "rev-list",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--merges"}},
			Args:  []string{fmt.Sprintf("%s..%s", tip, commitID)},
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return structerr.NewInternal("listing merge commits: %w", err).WithMetadata("stderr", stderr.String())
	}

	if strings.TrimSpace(stdout.String()) != "" {
		return structerr.NewInvalidArgument("commit range contains merge commits, which cannot be rebased: use STRATEGY_MERGE")
	}

	return nil
}

// hasMergeBase reports whether a and b have a common ancestor. Unrelated histories have no merge
// base, which git merge-base reports with exit status 1.
func hasMergeBase(ctx context.Context, repo *localrepo.Repo, a, b git.ObjectID) (bool, error) {
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "merge-base",
			Args: []string{a.String(), b.String()},
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		if status, ok := command.ExitStatus(err); ok && status == 1 {
			return false, nil
		}

		return false, fmt.Errorf("merge-base: %w, stderr: %q", err, stderr.String())
	}

	return true, nil
}

// updateLandedReference performs the compare-and-swap of the branch. Hook rejections are returned
// as PermissionDenied errors, lost races as landConflict and invalid requests or conflicting
// references as InvalidArgument or FailedPrecondition. Everything else is Internal.
func (s *Server) updateLandedReference(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	quarantineDir *quarantine.Dir,
	quarantineRepo *localrepo.Repo,
	reference git.ReferenceName,
	oldOID, newOID git.ObjectID,
	rewritten bool,
) error {
	if rewritten {
		ctx = changelog.WithRewritten(ctx)
	}

	// On MVCC repositories a concurrent writer is detected when the snapshot is committed rather
	// than by the compare-and-swap above. The commit then returns the registered error.
	if cache := mvcc.CacheFromContext(ctx); cache != nil {
		cache.SetCommitConflictError(
			structerr.NewFailedPrecondition("unexpected old value").WithDetail(
				landReferenceUpdateError(reference, oldOID, newOID),
			),
		)
	}

	err := s.updateReferenceWithHooks(ctx, in.GetRepository(), in.GetUser(), quarantineDir, reference, newOID, oldOID)
	if err == nil {
		return nil
	}

	var customHookErr updateref.CustomHookError
	if errors.As(err, &customHookErr) {
		return structerr.NewPermissionDenied("%w", customHookErr).WithDetail(&gitalypb.UserLandError{
			Error: &gitalypb.UserLandError_CustomHook{
				CustomHook: customHookErr.Proto(),
			},
		})
	}

	var notAllowedErr hook.NotAllowedError
	if errors.As(err, &notAllowedErr) {
		return structerr.NewPermissionDenied("%w", notAllowedErr).WithDetail(&gitalypb.UserLandError{
			Error: &gitalypb.UserLandError_AccessCheck{
				AccessCheck: &gitalypb.AccessCheckError{
					ErrorMessage: notAllowedErr.Message,
					UserId:       notAllowedErr.UserID,
					Protocol:     notAllowedErr.Protocol,
					Changes:      notAllowedErr.Changes,
				},
			},
		})
	}

	// Only lost races are retried. The other errors do not change when the attempt is repeated,
	// so retrying would only run the hooks again.
	var (
		mismatchingStateErr gitupdateref.MismatchingStateError
		alreadyLockedErr    gitupdateref.AlreadyLockedError
		alreadyExistsErr    gitupdateref.ReferenceAlreadyExistsError
		unexpectedOldValErr refdb.UnexpectedOldValueError
	)
	if errors.As(err, &mismatchingStateErr) ||
		errors.As(err, &alreadyLockedErr) ||
		errors.As(err, &alreadyExistsErr) ||
		errors.As(err, &unexpectedOldValErr) ||
		errors.Is(err, gitupdateref.ErrPackedRefsLocked) {
		return landConflict{
			reference: reference,
			oldOID:    oldOID,
			newOID:    newOID,
			cause:     err,
		}
	}

	var invalidFormatErr gitupdateref.InvalidReferenceFormatError
	if errors.As(err, &invalidFormatErr) {
		return structerr.NewInvalidArgument("invalid branch reference: %w", err)
	}

	var nonCommitErr gitupdateref.NonCommitObjectError
	if errors.As(err, &nonCommitErr) {
		return structerr.NewInvalidArgument("commit ID does not refer to a commit: %w", err)
	}

	var fileDirectoryErr gitupdateref.FileDirectoryConflictError
	if errors.As(err, &fileDirectoryErr) {
		return structerr.NewFailedPrecondition("reference conflicts with existing reference %q: %w", fileDirectoryErr.ExistingReferenceName, err)
	}

	// Git does not report every lost race in a form that is classified above. A branch that
	// was deleted concurrently fails with "unable to resolve reference", for example. When
	// locking or committing the reference failed and the branch no longer points to the tip
	// that was read, a concurrent writer moved it, so the attempt is retried. The next
	// attempt sees the current state of the branch.
	var updateErr updateref.Error
	if errors.As(err, &updateErr) {
		moved, resolveErr := landReferenceMoved(ctx, quarantineRepo, reference, oldOID)
		if resolveErr != nil {
			return structerr.NewInternal("updating ref with hooks: %w", errors.Join(err, resolveErr))
		}
		if moved {
			return landConflict{
				reference: reference,
				oldOID:    oldOID,
				newOID:    newOID,
				cause:     err,
			}
		}
	}

	return structerr.NewInternal("updating ref with hooks: %w", err)
}

// landReferenceMoved reports whether the reference no longer points to expectedOID. A missing
// reference has moved unless the zero OID was expected.
func landReferenceMoved(ctx context.Context, repo *localrepo.Repo, reference git.ReferenceName, expectedOID git.ObjectID) (bool, error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return false, fmt.Errorf("detecting object hash: %w", err)
	}

	current, err := repo.ResolveRevision(ctx, reference.Revision())
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return expectedOID != objectHash.ZeroOID, nil
		}

		return false, fmt.Errorf("resolving branch after failed update: %w", err)
	}

	return current != expectedOID, nil
}

// landMergeConflictError returns the FailedPrecondition error for a change that does not apply
// cleanly onto the branch tip.
func landMergeConflictError(tip, commitID git.ObjectID, conflictingFiles [][]byte, cause error) error {
	return structerr.NewFailedPrecondition("landing commit: %w", cause).WithDetail(&gitalypb.UserLandError{
		Error: &gitalypb.UserLandError_MergeConflict{
			MergeConflict: &gitalypb.MergeConflictError{
				ConflictingFiles:     conflictingFiles,
				ConflictingCommitIds: []string{tip.String(), commitID.String()},
			},
		},
	})
}

// landKeptMovingError returns the FailedPrecondition error for a landing that lost the
// compare-and-swap on every attempt, or that cannot be retried inside the RPC.
func landKeptMovingError(in *gitalypb.UserLandRequest, conflict landConflict, attempts int) error {
	return structerr.NewFailedPrecondition(
		"branch %q kept moving, giving up after %d attempts: %w",
		string(in.GetBranch()), attempts, conflict,
	).WithDetail(landReferenceUpdateError(conflict.reference, conflict.oldOID, conflict.newOID))
}

// landReferenceUpdateError returns the UserLandError detail for a failed compare-and-swap.
func landReferenceUpdateError(reference git.ReferenceName, oldOID, newOID git.ObjectID) *gitalypb.UserLandError {
	return &gitalypb.UserLandError{
		Error: &gitalypb.UserLandError_ReferenceUpdate{
			ReferenceUpdate: &gitalypb.ReferenceUpdateError{
				ReferenceName: []byte(reference),
				OldOid:        oldOID.String(),
				NewOid:        newOID.String(),
			},
		},
	}
}

// landResponse builds the UserLandResponse. Attempts is set by the caller.
func landResponse(oldTip, landedCommitID git.ObjectID, rewritten, alreadyLanded, created bool) *gitalypb.UserLandResponse {
	return &gitalypb.UserLandResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{
			CommitId:      landedCommitID.String(),
			BranchCreated: created,
		},
		LandedCommitId: landedCommitID.String(),
		Rewritten:      rewritten,
		AlreadyLanded:  alreadyLanded,
		OldTip:         oldTip.String(),
	}
}

// waitLandBackoff sleeps for a jittered, exponentially growing delay before the next attempt. The
// jitter spreads competing landers out so that they do not keep colliding.
func waitLandBackoff(ctx context.Context, attempt int) error {
	ceiling := min(landBackoffBase<<min(attempt-1, 6), landBackoffMax)
	delay := ceiling/2 + time.Duration(rand.Int64N(int64(ceiling/2)+1))

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateUserLandRequest(ctx context.Context, locator storage.Locator, in *gitalypb.UserLandRequest) error {
	if err := locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return err
	}

	if in.GetUser() == nil {
		return errors.New("empty user")
	}

	if len(in.GetUser().GetName()) == 0 {
		return errors.New("empty user name")
	}

	if len(in.GetUser().GetEmail()) == 0 {
		return errors.New("empty user email")
	}

	if len(in.GetBranch()) == 0 {
		return errors.New("empty branch name")
	}

	if strings.HasPrefix(string(in.GetBranch()), "refs/") {
		return errors.New("branch must be a branch name, not a full reference name")
	}

	if err := git.ValidateRevision(in.GetBranch()); err != nil {
		return fmt.Errorf("invalid branch name: %w", err)
	}

	// git refuses to create a branch called HEAD, and ValidateReference does not see it as a
	// branch because it only checks the full name.
	if string(in.GetBranch()) == "HEAD" {
		return errors.New("invalid branch name: HEAD is reserved")
	}

	if err := git.ValidateReference("refs/heads/" + string(in.GetBranch())); err != nil {
		return fmt.Errorf("invalid branch name: %w", err)
	}

	if in.GetCommitId() == "" {
		return errors.New("empty commit id")
	}

	if _, ok := gitalypb.UserLandRequest_Strategy_name[int32(in.GetStrategy())]; !ok {
		return fmt.Errorf("unknown strategy: %d", in.GetStrategy())
	}

	if in.GetMaxAttempts() < 0 {
		return errors.New("negative max attempts")
	}

	return nil
}
