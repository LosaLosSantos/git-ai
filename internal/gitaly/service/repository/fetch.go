package repository

import (
	"context"
	"errors"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/remoterepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func validateFetchSourceBranchRequest(ctx context.Context, locator storage.Locator, in *gitalypb.FetchSourceBranchRequest) error {
	if err := locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return err
	}
	if err := git.ValidateRevision(in.GetSourceBranch()); err != nil {
		return err
	}
	if err := git.ValidateRevision(in.GetTargetRef()); err != nil {
		return err
	}
	return nil
}

func (s *server) FetchSourceBranch(ctx context.Context, req *gitalypb.FetchSourceBranchRequest) (*gitalypb.FetchSourceBranchResponse, error) {
	if err := validateFetchSourceBranchRequest(ctx, s.locator, req); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	quarantineDir, targetRepo, cleanup, err := s.quarantinedRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	defer cleanup()

	sourceRepo, err := remoterepo.New(ctx, req.GetSourceRepository(), s.conns)
	if err != nil {
		return nil, structerr.NewInternal("%w", err)
	}

	var sourceOid git.ObjectID
	var containsObject bool

	// If source and target repository are the same, then we know that both
	// are local. We can thus optimize and locally resolve the reference
	// instead of using an RPC call. We also know that we can always skip
	// the fetch as the object will be available.
	if storage.RepoPathEqual(req.GetRepository(), req.GetSourceRepository()) {
		var err error

		sourceOid, err = targetRepo.ResolveRevision(ctx, git.Revision(req.GetSourceBranch()))
		if err != nil {
			if errors.Is(err, git.ErrReferenceNotFound) {
				return &gitalypb.FetchSourceBranchResponse{Result: false}, nil
			}
			return nil, structerr.NewInternal("%w", err)
		}

		containsObject = true
	} else {
		var err error

		sourceOid, err = sourceRepo.ResolveRevision(ctx, git.Revision(req.GetSourceBranch()))
		if err != nil {
			if errors.Is(err, git.ErrReferenceNotFound) {
				return &gitalypb.FetchSourceBranchResponse{Result: false}, nil
			}
			return nil, structerr.NewInternal("%w", err)
		}

		// Otherwise, if the source is a remote repository, we check
		// whether the target repo already contains the desired object.
		// If so, we can skip the fetch.
		containsObject, err = targetRepo.HasRevision(ctx, sourceOid.Revision()+"^{commit}")
		if err != nil {
			return nil, structerr.NewInternal("%w", err)
		}
	}

	// There's no need to perform the fetch if we already have the object
	// available.
	if !containsObject {
		fetchOpts := localrepo.FetchOpts{
			Tags: localrepo.FetchOptsTagsNone,
		}

		if featureflag.FetchSourceBranchNegotiationTips.IsEnabled(ctx) {
			// Limit the 'have' lines to branches and the target ref,
			// so internal refs (keep-around, pipelines, etc) are ignored
			// and re-pushes to an existing MR only fetch the delta.
			fetchOpts.NegotiationTips = []string{
				"refs/heads/*",
				string(req.GetTargetRef()),
			}
		}

		if featureflag.FetchSourceBranchHideRefs.IsEnabled(ctx) {
			fetchOpts.HideInternalRefs = true
		}

		if err := targetRepo.FetchInternal(
			ctx,
			req.GetSourceRepository(),
			[]string{sourceOid.String()},
			fetchOpts,
		); err != nil {
			// Design quirk: if the fetch fails, this RPC returns Result: false, but no error.
			if errors.As(err, &localrepo.FetchFailedError{}) {
				s.logger.
					WithField("oid", sourceOid.String()).
					WithError(err).WarnContext(ctx, "git fetch failed")
				return &gitalypb.FetchSourceBranchResponse{Result: false}, nil
			}

			return nil, err
		}
	}

	if err := quarantineDir.Migrate(ctx); err != nil {
		return nil, structerr.NewInternal("migrating quarantined objects: %w", err)
	}

	origTargetRepo := s.localRepoFactory.Build(req.GetRepository())
	if err := origTargetRepo.UpdateRef(ctx,
		git.ReferenceName(req.GetTargetRef()),
		sourceOid,
		git.ObjectID(req.GetExpectedTargetOldOid()),
	); err != nil {
		if errors.Is(err, localrepo.ErrMismatchingState) {
			return nil, structerr.NewFailedPrecondition("%w", err)
		}

		return nil, err
	}

	return &gitalypb.FetchSourceBranchResponse{Result: true}, nil
}
