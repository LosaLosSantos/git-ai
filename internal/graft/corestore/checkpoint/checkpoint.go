// Package checkpoint consolidates the history of a repository in the storage
// core: a checkpoint at sequence N is one self-contained pack of every object
// reachable from the references at N, with the reference snapshot at N, so that
// hydration starts from it instead of changeset 1 and garbage collection can
// delete the packs it covers (doc/graft/PLAN7.md, "Checkpoint creation",
// binding). The open reviews' commits are references too (their keep-around
// references, REVIEW ROOTS RULE), so the references are the whole closure.
//
// The build reads the database, catches the local repository up (EnsureFresh)
// and packs from it; it holds no reference lock and never blocks or fails a
// write (CKPT-2). A build that fails at any step records nothing; a pack it
// uploaded is an orphan that garbage collection reclaims. A build at a sequence
// that has a checkpoint already returns that checkpoint.
package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Fault points of a build, for crash injection in tests.
const (
	// PointAfterUpload is reached when the pack is uploaded and no row
	// references it yet.
	PointAfterUpload = "after_upload"
	// PointAfterCommit is reached when the checkpoint is recorded.
	PointAfterCommit = "after_commit"
)

// Checkpointer builds and records checkpoints.
type Checkpointer struct {
	core        *corestore.Core
	logger      log.Logger
	locator     storage.Locator
	repoFactory localrepo.Factory
	fresh       corestore.Freshener

	// faultHook, when set by a test, is called at the fault points; an error it
	// returns ends the build there, as a crash would. Nil in production.
	faultHook func(point string) error
}

// New returns the checkpointer of core. fresh catches the local repositories
// up before a build.
func New(core *corestore.Core, logger log.Logger, locator storage.Locator, repoFactory localrepo.Factory, fresh corestore.Freshener) *Checkpointer {
	return &Checkpointer{
		core:        core,
		logger:      logger,
		locator:     locator,
		repoFactory: repoFactory,
		fresh:       fresh,
	}
}

// Create builds a checkpoint of repo at the current database head and records
// it. created is false, with the existing checkpoint, when a checkpoint at the
// head exists already, and false with a zero Checkpoint when the repository has
// no object to consolidate (every reference is symbolic). The errors carry gRPC
// codes: FailedPrecondition outside authoritative mode and for an incomplete
// import; NotFound for an untracked repository; Unavailable for the database
// and the bucket; Internal for a broken invariant.
func (c *Checkpointer) Create(ctx context.Context, repo *gitalypb.Repository) (corestore.Checkpoint, bool, error) {
	if !c.core.Authoritative() {
		return corestore.Checkpoint{}, false, structerr.NewFailedPrecondition("checkpoints are created in authoritative mode only")
	}

	key := corestore.KeyOf(repo)
	record, err := c.core.Meta.GetRepository(ctx, key)
	if err != nil {
		return corestore.Checkpoint{}, false, errRepository(err)
	}
	if record.HeadSeq == 0 {
		return corestore.Checkpoint{}, false, structerr.NewFailedPrecondition("the import of the repository into the storage core is not complete")
	}
	logger := c.logger.WithFields(log.Fields{
		"repository":    key.String(),
		"repository_id": record.ID,
	})

	// Step 1: the reference snapshot at N, and the checkpoint at N if there is
	// one.
	refs, seq, err := c.core.Meta.Refs(ctx, record.ID)
	if err != nil {
		return corestore.Checkpoint{}, false, errRepository(err)
	}
	newest, ok, err := c.core.Meta.NewestCheckpoint(ctx, record.ID)
	if err != nil {
		return corestore.Checkpoint{}, false, errRepository(err)
	}
	if ok && newest.Seq == seq {
		return newest, false, nil
	}

	// The local repository may not exist before step 2 (EnsureFresh hydrates
	// it in the format of the record).
	hash, err := git.ObjectHashByFormat(record.ObjectFormat)
	if err != nil {
		return corestore.Checkpoint{}, false, structerr.NewInternal("object format of the repository in the storage core: %w", err)
	}
	tips, err := objectTips(refs, hash)
	if err != nil {
		return corestore.Checkpoint{}, false, err
	}
	if len(tips) == 0 {
		return corestore.Checkpoint{}, false, nil
	}

	// Step 2 (change D): catch the local repository up to a head read after
	// the call, so at least N.
	if err := c.fresh.EnsureFresh(ctx, repo); err != nil {
		return corestore.Checkpoint{}, false, err
	}
	wm, ok, err := c.fresh.Applied(ctx, repo)
	if err != nil {
		return corestore.Checkpoint{}, false, structerr.NewInternal("read the local watermark: %w", err)
	}
	if !ok || wm.RepoID != record.ID || wm.Seq < seq {
		// EnsureFresh succeeded, so the local repository is at a head read after
		// this call arrived: anything else breaks the Freshener contract.
		return corestore.Checkpoint{}, false, structerr.NewInternal("the local repository is not caught up after EnsureFresh: watermark %+v (present %t), repository %d at changeset %d",
			wm, ok, record.ID, seq)
	}

	// Step 3. After EnsureFresh the local repository holds every object of the
	// snapshot; a missing one fails the build (never a partial pack).
	local := c.repoFactory.Build(repo)
	tmpDir, err := c.tempDir(repo.GetStorageName())
	if err != nil {
		return corestore.Checkpoint{}, false, structerr.NewInternal("temporary directory: %w", err)
	}
	pack, err := gitops.BuildPackFrom(ctx, local, tips, tmpDir)
	if err != nil {
		return corestore.Checkpoint{}, false, structerr.NewInternal("build the checkpoint pack: %w", err)
	}
	defer func() {
		if err := pack.Remove(); err != nil {
			logger.WithError(err).Warn("storage core: removing the temporary checkpoint pack")
		}
	}()

	// Step 4: the write deadline starts at a read of the record taken right
	// before the upload (PACK OWNERSHIP RULE 4).
	readAt := time.Now()
	current, err := c.core.Meta.GetRepository(ctx, key)
	if err != nil {
		return corestore.Checkpoint{}, false, errRepository(err)
	}
	if current.ID != record.ID {
		return corestore.Checkpoint{}, false, errRemoved()
	}
	f, err := pack.Open()
	if err != nil {
		return corestore.Checkpoint{}, false, structerr.NewInternal("open the checkpoint pack: %w", err)
	}
	upload, err := c.core.UploadPack(ctx, record.ID, readAt, pack.SHA256, pack.Size, f)
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		return corestore.Checkpoint{}, false, structerr.NewUnavailable("storage core: upload the checkpoint pack: %w", err)
	}
	if err := c.fault(PointAfterUpload); err != nil {
		return corestore.Checkpoint{}, false, err
	}

	// Step 5.
	commitCtx, cancel, err := upload.CommitContext(ctx)
	if err != nil {
		return corestore.Checkpoint{}, false, structerr.NewUnavailable("%w", err)
	}
	defer cancel()

	cp, created, err := c.core.Meta.CreateCheckpoint(commitCtx, record.ID, corestore.CheckpointInput{
		Seq:       seq,
		Refs:      refs,
		PackKey:   upload.Key,
		PackBytes: upload.Bytes,
	})
	if err != nil {
		return corestore.Checkpoint{}, false, errRepository(err)
	}
	if err := c.fault(PointAfterCommit); err != nil {
		return corestore.Checkpoint{}, false, err
	}

	if created {
		logger.WithFields(log.Fields{
			"sequence":   cp.Seq,
			"pack_key":   cp.PackKey,
			"pack_bytes": cp.PackBytes,
		}).Info("storage core: checkpoint created")
	}
	return cp, created, nil
}

// objectTips returns the object IDs the references point to (symbolic
// references are skipped), each validated against the repository's hash.
func objectTips(refs map[string]corestore.Target, hash git.ObjectHash) ([]git.ObjectID, error) {
	tips := make([]git.ObjectID, 0, len(refs))
	for name, target := range refs {
		if target.IsSymbolic() {
			continue
		}
		if err := hash.ValidateHex(string(target)); err != nil {
			return nil, structerr.NewInternal("reference %q in the storage core: %w", name, err)
		}
		tips = append(tips, git.ObjectID(target))
	}
	slices.Sort(tips)
	return slices.Compact(tips), nil
}

// tempDir returns the temporary directory of the storage, created if missing.
func (c *Checkpointer) tempDir(storageName string) (string, error) {
	dir, err := c.locator.TempDir(storageName)
	if err != nil {
		return "", err
	}
	// The server creates it when it starts, but it may have been removed since.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func (c *Checkpointer) fault(point string) error {
	if c.faultHook == nil {
		return nil
	}
	if err := c.faultHook(point); err != nil {
		return fmt.Errorf("fault at %s: %w", point, err)
	}
	return nil
}

// errRepository maps a database error of the build.
func errRepository(err error) error {
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return structerr.NewNotFound("the repository is not tracked by the storage core")
	}
	return structerr.NewUnavailable("storage core: %w", err)
}

// errRemoved is the error of a build whose repository was removed (and
// possibly created again under a new ID) while it ran.
func errRemoved() error {
	return structerr.NewFailedPrecondition("the repository was removed from the storage core during the checkpoint build")
}
