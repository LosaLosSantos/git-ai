package storagecore

import (
	"context"
	"errors"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// GetStorageState implements gitalypb.StorageCoreServiceServer. It works when
// the storage core is off (mode "off", nothing tracked) and for a repository
// that this node has no local copy of (another node may have it).
func (s *server) GetStorageState(ctx context.Context, req *gitalypb.GetStorageStateRequest) (*gitalypb.GetStorageStateResponse, error) {
	repo := req.GetRepository()
	if err := s.deps.GetLocator().ValidateRepository(ctx, repo, storage.WithSkipRepositoryExistenceCheck()); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	core := s.deps.GetGraftStorage()
	resp := &gitalypb.GetStorageStateResponse{Mode: string(corestore.ModeOff)}
	if !core.Enabled() {
		return resp, nil
	}

	resp.Mode = string(core.Mode)
	resp.NodeId = core.NodeID

	record, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
	switch {
	case err == nil:
		resp.Tracked = true
		resp.RepositoryId = record.ID
		resp.HeadSequence = record.HeadSeq
	case errors.Is(err, corestore.ErrRepositoryNotFound):
		// Untracked: there is no head and no watermark of this repository.
		return resp, nil
	default:
		return nil, structerr.NewUnavailable("storage core: %w", err)
	}

	// The applied sequence is informative: the syncer reports a watermark only
	// in authoritative mode, and one of another repository ID (a stale local
	// copy) does not count. A watermark that cannot be read is logged, not
	// returned.
	w, ok, err := s.deps.GetGraftStorageSyncer().Applied(ctx, repo)
	switch {
	case err != nil:
		s.deps.GetLogger().WithError(err).Warn("storage core: reading the local watermark")
	case ok && w.RepoID == resp.RepositoryId:
		resp.AppliedSequence = w.Seq
	}

	return resp, nil
}
