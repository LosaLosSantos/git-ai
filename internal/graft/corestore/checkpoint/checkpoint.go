// Package checkpoint consolidates the history of a repository in the storage
// core: a checkpoint at sequence N is one self-contained pack of every object
// reachable from the references at N and from the open reviews' commits, with
// the reference snapshot at N, so that hydration starts from it instead of
// changeset 1 and garbage collection can delete the packs it covers
// (doc/graft/PLAN7.md, "Checkpoint creation", binding).
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

	"github.com/prometheus/client_golang/prometheus"

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
	// PointAfterSnapshot is reached when the reference snapshot is read and the
	// open reviews are not yet: a review written here has a snapshot greater
	// than the checkpoint's sequence.
	PointAfterSnapshot = "after_snapshot"
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

	reviewObjectsMissing prometheus.Counter
}

var _ prometheus.Collector = (*Checkpointer)(nil)

// New returns the checkpointer of core. fresh catches the local repositories
// up before a build.
func New(core *corestore.Core, logger log.Logger, locator storage.Locator, repoFactory localrepo.Factory, fresh corestore.Freshener) *Checkpointer {
	return &Checkpointer{
		core:        core,
		logger:      logger,
		locator:     locator,
		repoFactory: repoFactory,
		fresh:       fresh,
		reviewObjectsMissing: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_checkpoint_review_objects_missing_total",
			Help: "Number of checkpoint builds refused because a commit of an open review is missing from the local repository.",
		}),
	}
}

// Describe implements prometheus.Collector.
func (c *Checkpointer) Describe(descs chan<- *prometheus.Desc) {
	c.reviewObjectsMissing.Describe(descs)
}

// Collect implements prometheus.Collector.
func (c *Checkpointer) Collect(metrics chan<- prometheus.Metric) {
	c.reviewObjectsMissing.Collect(metrics)
}

// Create builds a checkpoint of repo at the current database head and records
// it. created is false, with the existing checkpoint, when a checkpoint at the
// head exists already, and false with a zero Checkpoint when the repository has
// no object to consolidate (every reference is symbolic and no review is
// open). The errors carry gRPC codes: FailedPrecondition outside authoritative
// mode, for an incomplete import, for an open review whose commits are missing
// locally (metadata review_id) and for reviews that changed during the build;
// NotFound for an untracked repository; Unavailable for the database and the
// bucket; Internal for a broken invariant.
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
	if err := c.fault(PointAfterSnapshot); err != nil {
		return corestore.Checkpoint{}, false, err
	}

	// The local repository may not exist before step 3 (EnsureFresh hydrates
	// it in the format of the record).
	hash, err := git.ObjectHashByFormat(record.ObjectFormat)
	if err != nil {
		return corestore.Checkpoint{}, false, structerr.NewInternal("object format of the repository in the storage core: %w", err)
	}

	// Step 2: the open reviews' commits (the roots), read after the snapshot,
	// then the head H. Every review read was written after its own check at a
	// snapshot S <= H (REVIEW ROOTS R1), so after step 3 the local repository
	// holds its commits, even those that changesets after N brought in.
	roots, err := c.reviewRoots(ctx, record.ID, hash)
	if err != nil {
		return corestore.Checkpoint{}, false, err
	}
	current, err := c.core.Meta.GetRepository(ctx, key)
	if err != nil {
		return corestore.Checkpoint{}, false, errRepository(err)
	}
	if current.ID != record.ID {
		return corestore.Checkpoint{}, false, errRemoved()
	}
	head := current.HeadSeq

	// Step 3 (change D): catch the local repository up to a head read after
	// the call, so at least H.
	if err := c.fresh.EnsureFresh(ctx, repo); err != nil {
		return corestore.Checkpoint{}, false, err
	}
	wm, ok, err := c.fresh.Applied(ctx, repo)
	if err != nil {
		return corestore.Checkpoint{}, false, structerr.NewInternal("read the local watermark: %w", err)
	}
	if !ok || wm.RepoID != record.ID || wm.Seq < head {
		// EnsureFresh succeeded, so the local repository is at a head read after
		// this call arrived: anything else breaks the Freshener contract.
		return corestore.Checkpoint{}, false, structerr.NewInternal("the local repository is not caught up after EnsureFresh: watermark %+v (present %t), repository %d at head %d",
			wm, ok, record.ID, head)
	}

	// Step 4: every object is present, or a precise error and nothing built.
	local := c.repoFactory.Build(repo)
	refTips, err := objectTips(refs, hash)
	if err != nil {
		return corestore.Checkpoint{}, false, err
	}
	tips := append(slices.Clone(refTips), roots.commits()...)
	missing, err := gitops.MissingObjects(ctx, local, tips)
	if err != nil {
		return corestore.Checkpoint{}, false, structerr.NewInternal("look up the objects: %w", err)
	}
	if err := c.checkMissing(ctx, logger, refTips, roots, missing); err != nil {
		return corestore.Checkpoint{}, false, err
	}

	// Step 5.
	if len(tips) == 0 {
		return corestore.Checkpoint{}, false, nil
	}
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

	// Step 6: the write deadline starts at a read of the record taken right
	// before the upload (PACK OWNERSHIP RULE 4).
	readAt := time.Now()
	current, err = c.core.Meta.GetRepository(ctx, key)
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

	// Step 7.
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
		Roots:     roots.strings(),
	})
	switch {
	case errors.Is(err, corestore.ErrCheckpointRootsChanged):
		return corestore.Checkpoint{}, false, structerr.NewFailedPrecondition("the reviews changed during the checkpoint build, run it again: %w", err)
	case err != nil:
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

// root is a commit of an open review.
type root struct {
	reviewID string
	commit   git.ObjectID
}

// rootSet is the root set of a build: the head and base commits of the open
// reviews, in the order read.
type rootSet []root

func (s rootSet) commits() []git.ObjectID {
	commits := make([]git.ObjectID, 0, len(s))
	for _, r := range s {
		commits = append(commits, r.commit)
	}
	return commits
}

func (s rootSet) strings() []string {
	commits := make([]string, 0, len(s))
	for _, r := range s {
		commits = append(commits, r.commit.String())
	}
	return commits
}

// reviewRoots reads the open reviews of the repository and returns their head
// and base commits; empty values are skipped. A commit that is not an object
// ID of the repository's hash cannot be packed, so the build is refused like
// one that is missing.
func (c *Checkpointer) reviewRoots(ctx context.Context, repoID int64, hash git.ObjectHash) (rootSet, error) {
	reviews, err := c.core.Meta.ListReviews(ctx, repoID, "", false, 0)
	if err != nil {
		return nil, errRepository(err)
	}

	var roots rootSet
	var invalid []string
	for _, review := range reviews {
		for _, commit := range []string{review.GetHeadCommitId(), review.GetBaseCommitId()} {
			if commit == "" {
				continue
			}
			if err := hash.ValidateHex(commit); err != nil {
				invalid = append(invalid, review.GetId())
				continue
			}
			roots = append(roots, root{reviewID: review.GetId(), commit: git.ObjectID(commit)})
		}
	}
	if len(invalid) > 0 {
		c.reviewObjectsMissing.Inc()
		return nil, structerr.NewFailedPrecondition("open reviews %v have a commit that is not an object ID of the repository; close them to create a checkpoint", invalid).
			WithMetadata("review_id", invalid)
	}
	return roots, nil
}

// checkMissing turns the objects missing from the local repository into the
// error of the build. A missing reference tip is impossible after EnsureFresh
// (Internal). A missing review commit belongs to a review written without the
// R1 check (created before phase 7, or imported from the shadow-mode files):
// FailedPrecondition naming the reviews.
func (c *Checkpointer) checkMissing(ctx context.Context, logger log.Logger, refTips []git.ObjectID, roots rootSet, missing []git.ObjectID) error {
	if len(missing) == 0 {
		return nil
	}

	absent := make(map[git.ObjectID]struct{}, len(missing))
	for _, oid := range missing {
		absent[oid] = struct{}{}
	}
	for _, tip := range refTips {
		if _, ok := absent[tip]; ok {
			return structerr.NewInternal("object %s of a reference is missing from the local repository after EnsureFresh", tip)
		}
	}

	var reviewIDs []string
	for _, r := range roots {
		if _, ok := absent[r.commit]; ok && !slices.Contains(reviewIDs, r.reviewID) {
			reviewIDs = append(reviewIDs, r.reviewID)
		}
	}
	c.reviewObjectsMissing.Inc()
	logger.WithField("review_id", reviewIDs).WarnContext(ctx, "storage core: a checkpoint is refused: commits of open reviews are missing")
	return structerr.NewFailedPrecondition("commits of open reviews %v are missing from the repository; close them to create a checkpoint", reviewIDs).
		WithMetadata("review_id", reviewIDs)
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
