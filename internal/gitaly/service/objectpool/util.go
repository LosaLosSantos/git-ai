package objectpool

import (
	"context"
	"errors"

	"github.com/feder-cr/open_scion/internal/git"
	"github.com/feder-cr/open_scion/internal/git/objectpool"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

var (
	errInvalidPoolDir = structerr.NewInvalidArgument("%w", objectpool.ErrInvalidPoolDir)

	// errMissingPool is returned when the request is missing the object pool.
	errMissingPool = structerr.NewInvalidArgument("no object pool repository")

	// errMVCCReferenceBackend is returned when a repository using the MVCC reference backend
	// is used to create or modify an object pool.
	errMVCCReferenceBackend = structerr.NewInvalidArgument("object pools are not supported for repositories using the MVCC reference backend")
)

// PoolRequest is the interface of a gRPC request that carries an object pool.
type PoolRequest interface {
	GetObjectPool() *gitalypb.ObjectPool
}

// ExtractPool returns the pool repository from the request or an error if the
// request did no contain a pool.
func ExtractPool(req PoolRequest) (*gitalypb.Repository, error) {
	poolRepo := req.GetObjectPool().GetRepository()
	if poolRepo == nil {
		return nil, errMissingPool
	}

	return poolRepo, nil
}

// validateReferenceBackend returns errMVCCReferenceBackend when the given repository uses the
// MVCC reference backend. Repositories using the MVCC reference backend cannot be used for object
// pools as they stand today.
func (s *server) validateReferenceBackend(ctx context.Context, repo *gitalypb.Repository) error {
	backend, err := s.localRepoFactory.Build(repo).ReferenceBackend(ctx)
	if err != nil {
		return structerr.New("detecting reference backend: %w", err)
	}

	if backend.Name == git.ReferenceBackendMVCC.Name {
		return errMVCCReferenceBackend
	}

	return nil
}

func (s *server) poolForRequest(ctx context.Context, req PoolRequest) (*objectpool.ObjectPool, error) {
	pool, err := objectpool.FromProto(ctx, s.logger, s.locator, s.gitCmdFactory, s.catfileCache, s.txManager, s.housekeepingManager, req.GetObjectPool())
	if err != nil {
		if errors.Is(err, objectpool.ErrInvalidPoolDir) {
			return nil, errInvalidPoolDir
		}

		if errors.Is(err, objectpool.ErrInvalidPoolRepository) {
			return nil, structerr.NewFailedPrecondition("%w", err)
		}

		return nil, structerr.NewInternal("%w", err)
	}

	return pool, nil
}
