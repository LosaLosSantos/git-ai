// Package syncer keeps the local git repositories of a node, which are caches
// in authoritative mode, up to date with the storage core: catch-up, hydration
// and the local watermark. Owned by WP55 (doc/graft/PLAN6.md, "Read path and
// catch-up", binding).
//
// EnsureFresh runs before a request is served (never inside a hook). It reads
// the repository from the database and then either catches the local
// repository up from its watermark to the database head (downloading and
// indexing each changeset's pack, then moving the changed references to their
// final values without hooks) or hydrates a fresh bare reftable repository
// from changeset 1 and renames it into place. A local repository of another
// repository ID, without a valid watermark, invalid, or not using the reftable
// backend is a stale cache: it is moved away and replaced by a hydration,
// unless it holds file changelog entries or file reviews (written in shadow
// mode) that the database does not have: such a copy is left in place and
// EnsureFresh fails with FailedPrecondition until ImportRepository brought
// that history in (checkReplaceable). A local repository with a watermark
// whose key the database does not have is the stale cache of a removed
// repository: it is moved away and EnsureFresh returns NotFound (untracked).
//
// Catch-up never reverts a local write (see catchUp). It reads the local
// values of the changed references (immediate values: a symbolic reference is
// its own target, not the end of its chain) and accepts each only if the range
// explains it; an unexplained value makes it read the database head again and
// extend the range, or repair the reference when the head did not move. It
// extends the range over any newer changeset of this node that touched a
// reference it is about to move, then runs update-ref transactions in which git
// checks, under its reference locks, that every reference of the range still
// has the value read (applyMoves): the references not yet final are moved, the
// others only verified, never rewritten. A local write committed in between
// fails the transaction, and so does one that was between git's prepared and
// committed states (the transaction waits for its lock); the check then runs
// again. Then it writes the watermark.
//
// The per-repository mutex is held only around local work (applying references,
// writing the watermark, renaming the repository): the database and the bucket
// are never waited for while it is held, so that NoteCommitted, called from the
// reference-transaction hook, can always take it without waiting for the
// network (RULE 6).
package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/singleflight"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	reviewstore "github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	defaultSyncTimeout = 10 * time.Second
	// changesetBatch is how many changesets are listed from the database at once.
	changesetBatch = 500
	// reportInterval is the minimum time between two ReportApplied calls for one repository.
	reportInterval = time.Second
	initialBackoff = 10 * time.Millisecond
	maxBackoff     = 500 * time.Millisecond

	resultFresh     = "fresh"
	resultCaughtUp  = "caught_up"
	resultHydrated  = "hydrated"
	resultError     = "error"
	tmpPackPattern  = "graft-sync-pack-*"
	tmpHydratePatt  = "hydrate-*"
	tmpStalePattern = "stale-*"
)

// errRestart makes a sync start over: the local repository changed while the
// repository lock was not held (another repository ID, or no watermark).
var errRestart = errors.New("storage core: local repository changed during the sync")

// Syncer implements corestore.Freshener and corestore.LocalApplier.
type Syncer struct {
	core          *corestore.Core
	logger        log.Logger
	locator       storage.Locator
	repoFactory   localrepo.Factory
	gitCmdFactory gitcmd.CommandFactory

	// flight makes concurrent EnsureFresh calls of one repository share one sync.
	flight singleflight.Group
	// headReads hands out the tickets of the syncs: a sync takes one just before
	// it reads the database head (see EnsureFresh).
	headReads atomic.Uint64

	// testHook, when set by a test, is called at named points of a catch-up
	// ("fetched": the range is downloaded, nothing checked yet; "checked": the
	// local references are checked, the apply follows). Nil in production.
	testHook func(point string)

	// mu protects the maps below. It is never held while waiting for a
	// repository lock.
	mu sync.Mutex
	// locks is the per-repository mutex (a buffered channel of one slot, so that
	// it can be tried without blocking), keyed by the local repository path.
	// Entries are removed when no goroutine refers to them.
	locks map[string]*repoLock
	// pending are the changeset sequences committed by this node whose local
	// effects are already in the local repository, keyed by repository path.
	pending map[string]*pendingSeqs
	// reported is the last ReportApplied time per repository ID.
	reported map[int64]time.Time

	syncTotal      *prometheus.CounterVec
	syncSeconds    prometheus.Histogram
	syncChangesets prometheus.Counter
	syncBytes      prometheus.Counter
	syncSeeds      prometheus.Counter
}

type repoLock struct {
	sem  chan struct{}
	refs int
}

type pendingSeqs struct {
	repoID int64
	seqs   map[uint64]struct{}
}

var (
	_ corestore.Freshener    = (*Syncer)(nil)
	_ corestore.LocalApplier = (*Syncer)(nil)
	_ prometheus.Collector   = (*Syncer)(nil)
)

// New returns the syncer of core.
func New(core *corestore.Core, logger log.Logger, locator storage.Locator, repoFactory localrepo.Factory, gitCmdFactory gitcmd.CommandFactory) *Syncer {
	s := &Syncer{
		core:          core,
		logger:        logger,
		locator:       locator,
		repoFactory:   repoFactory,
		gitCmdFactory: gitCmdFactory,
		locks:         map[string]*repoLock{},
		pending:       map[string]*pendingSeqs{},
		reported:      map[int64]time.Time{},
		syncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graft_storage_sync_total",
			Help: "Number of storage core syncs before a request, by result (fresh, caught_up, hydrated, error).",
		}, []string{"result"}),
		syncSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "graft_storage_sync_seconds",
			Help:    "Duration of storage core syncs before a request.",
			Buckets: prometheus.DefBuckets,
		}),
		syncChangesets: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_sync_changesets_total",
			Help: "Number of changesets applied to local repositories by catch-up and hydration.",
		}),
		syncBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_sync_bytes_total",
			Help: "Bytes of packs downloaded by catch-up and hydration.",
		}),
		syncSeeds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_sync_checkpoint_seeds_total",
			Help: "Number of hydrations that started from a checkpoint instead of changeset 1.",
		}),
	}
	for _, result := range []string{resultFresh, resultCaughtUp, resultHydrated, resultError} {
		s.syncTotal.WithLabelValues(result)
	}
	return s
}

// EnsureFresh implements corestore.Freshener.
//
// Concurrent calls for one repository share one sync (single flight). The
// sync runs under its own sync_timeout budget, detached from the cancellation
// of the caller that started it, so that a caller that goes away does not fail
// the others; every caller still stops waiting when its own context is done.
func (s *Syncer) EnsureFresh(ctx context.Context, repo storage.Repository) error {
	if s == nil || !s.core.Authoritative() {
		return nil
	}

	timeout := s.syncTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	key := corestore.KeyOf(repo)
	flightCtx := context.WithoutCancel(ctx)

	// A call must not be answered by a sync that read the database head before
	// the call arrived: a changeset committed (on another node) between that read
	// and the call would not be seen, breaking read-your-writes across nodes.
	// Every sync takes a ticket from a global counter just before it reads the
	// head; a sync whose ticket is not newer than the counter at arrival is
	// joined, but then followed by one more (at most two rounds: the next flight
	// of the key starts after the joined one ended, so after this call arrived).
	arrived := s.headReads.Load()
	for {
		ch := s.flight.DoChan(key.String(), func() (any, error) {
			syncCtx, cancel := context.WithTimeout(flightCtx, timeout)
			defer cancel()

			ticket := s.headReads.Add(1)
			err := s.sync(syncCtx, key)
			if err != nil && syncCtx.Err() != nil {
				// The budget ran out (lock contention, a slow database or bucket).
				return ticket, structerr.NewUnavailable("storage core: sync of %s did not finish within %s: %v", key, timeout, err)
			}
			return ticket, err
		})

		select {
		case res := <-ch:
			if res.Err != nil {
				return res.Err
			}
			if ticket, ok := res.Val.(uint64); ok && ticket > arrived {
				return nil
			}
			// The joined sync read the head before this call arrived: sync again.
		case <-ctx.Done():
			return structerr.NewUnavailable("storage core: waiting for the sync of %s: %w", key, ctx.Err())
		}
	}
}

// Applied implements corestore.Freshener.
func (s *Syncer) Applied(ctx context.Context, repo storage.Repository) (corestore.Watermark, bool, error) {
	if s == nil || !s.core.Authoritative() {
		return corestore.Watermark{}, false, nil
	}

	path, err := s.locator.GetRepoPath(ctx, repo, storage.WithRepositoryVerificationSkipped())
	if err != nil {
		return corestore.Watermark{}, false, err
	}

	w, ok, err := gitops.ReadWatermark(path)
	if err != nil {
		return corestore.Watermark{}, false, fmt.Errorf("read watermark: %w", err)
	}
	return w, ok, nil
}

// NoteCommitted implements corestore.LocalApplier. It never blocks: when a
// sync holds the repository lock, the sequence is only remembered and the
// next EnsureFresh advances the watermark through it.
//
// Outside authoritative mode it does nothing: no EnsureFresh would ever drop
// the remembered sequences (they would accumulate for the life of the
// process), and a watermark write must not delay a shadow write (RULE 4).
func (s *Syncer) NoteCommitted(ctx context.Context, repoPath string, repoID int64, seq uint64) {
	if s == nil || !s.core.Authoritative() {
		return
	}

	s.mu.Lock()
	p := s.pending[repoPath]
	if p == nil || p.repoID != repoID {
		p = &pendingSeqs{repoID: repoID, seqs: map[uint64]struct{}{}}
		s.pending[repoPath] = p
	}
	p.seqs[seq] = struct{}{}
	s.mu.Unlock()

	release, ok := s.tryLock(repoPath)
	if !ok {
		return
	}
	defer release()

	wm, ok, err := gitops.ReadWatermark(repoPath)
	if err != nil || !ok || wm.RepoID != repoID {
		return
	}
	s.advanceLocked(repoPath, wm)
}

// Describe implements prometheus.Collector (the graft_storage_* metrics of
// the catch-up).
func (s *Syncer) Describe(descs chan<- *prometheus.Desc) {
	if s == nil {
		return
	}
	s.syncTotal.Describe(descs)
	s.syncSeconds.Describe(descs)
	s.syncChangesets.Describe(descs)
	s.syncBytes.Describe(descs)
	s.syncSeeds.Describe(descs)
}

// Collect implements prometheus.Collector.
func (s *Syncer) Collect(metrics chan<- prometheus.Metric) {
	if s == nil {
		return
	}
	s.syncTotal.Collect(metrics)
	s.syncSeconds.Collect(metrics)
	s.syncChangesets.Collect(metrics)
	s.syncBytes.Collect(metrics)
	s.syncSeeds.Collect(metrics)
}

func (s *Syncer) syncTimeout() time.Duration {
	if s.core != nil && s.core.SyncTimeout > 0 {
		return s.core.SyncTimeout
	}
	return defaultSyncTimeout
}

// sync brings the local repository of key up to date. It records the metrics
// of the sync; the singleflight leader runs it once for concurrent callers.
func (s *Syncer) sync(ctx context.Context, key corestore.RepoKey) error {
	start := time.Now()
	result := resultError
	defer func() {
		s.syncSeconds.Observe(time.Since(start).Seconds())
		s.syncTotal.WithLabelValues(result).Inc()
	}()

	repo := &gitalypb.Repository{StorageName: key.Storage, RelativePath: key.RelativePath}

	path, err := s.locator.GetRepoPath(ctx, repo, storage.WithRepositoryVerificationSkipped())
	if err != nil {
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return structerr.NewUnavailable("storage core: sync: %w", err)
		}

		// The record is read again on every round: a restart can mean that the
		// repository was removed and registered again under a new ID meanwhile, and
		// a round with the old record would never converge.
		r, hash, err := s.record(ctx, key, repo)
		if errors.Is(err, errRestart) {
			continue
		}
		if err != nil {
			return err
		}

		wm, ok := s.readWatermark(path)
		stale := !ok || wm.RepoID != r.ID || !s.localCurrent(ctx, repo, path)
		// boundary is C_sync: a local repository below it is hydrated, because
		// the changeset packs it would catch up from may be collected
		// (doc/graft/PLAN7.md, GC BOUNDARY rule, change C).
		var boundary uint64
		if !stale {
			// Advance through the sequences this node committed itself before
			// downloading anything.
			wm, err = s.advance(ctx, path, wm)
			if err != nil {
				return err
			}
			if wm.Seq >= r.HeadSeq {
				result = resultFresh
				return nil
			}
			boundary, err = s.syncBoundary(ctx, r.ID)
			if errors.Is(err, errRestart) {
				continue
			}
			if err != nil {
				return err
			}
			stale = wm.Seq < boundary
		}

		if stale {
			if err := s.checkReplaceable(ctx, r, path, wm, ok); err != nil {
				return err
			}
			err := s.hydrate(ctx, repo, r, hash, path, boundary)
			if errors.Is(err, errRestart) {
				continue
			}
			if err != nil {
				return err
			}
			result = resultHydrated
			return nil
		}

		fresh, err := s.catchUp(ctx, repo, r, hash, path, wm.Seq)
		if errors.Is(err, errRestart) {
			continue
		}
		if err != nil {
			return err
		}
		if fresh {
			result = resultFresh
		} else {
			result = resultCaughtUp
		}
		return nil
	}
}

// syncBoundary returns C_sync of the repository: the sequence of the newest
// checkpoint created at least gc_grace − sync_timeout − gc_margin ago on the
// database clock, 0 when there is none (doc/graft/PLAN7.md, GC BOUNDARY rule).
// A sync round, bounded by sync_timeout, never needs a pack that garbage
// collection deletes meanwhile when its watermark is at or above C_sync. The
// configuration requires gc_grace > sync_timeout + gc_margin; with times that
// do not (only test cores), the age is 0 and every checkpoint counts, which is
// the conservative end: more hydrations, never a collected pack.
func (s *Syncer) syncBoundary(ctx context.Context, repoID int64) (uint64, error) {
	age := max(s.core.GCGrace-s.syncTimeout()-s.core.GCMargin, 0)
	boundary, err := s.core.Meta.SyncBoundary(ctx, repoID, age)
	if err != nil {
		if errors.Is(err, corestore.ErrRepositoryNotFound) {
			return 0, errRestart
		}
		return 0, structerr.NewUnavailable("storage core: sync boundary: %w", err)
	}
	return boundary, nil
}

// record reads the database record of key and the object hash of its format.
// For a key the database does not have it returns the error of untracked
// (errRestart when the repository was registered meanwhile).
func (s *Syncer) record(ctx context.Context, key corestore.RepoKey, repo *gitalypb.Repository) (corestore.Repository, git.ObjectHash, error) {
	r, err := s.core.Meta.GetRepository(ctx, key)
	if err != nil {
		if !errors.Is(err, corestore.ErrRepositoryNotFound) {
			return corestore.Repository{}, git.ObjectHash{}, structerr.NewUnavailable("storage core: get repository: %w", err)
		}
		return corestore.Repository{}, git.ObjectHash{}, s.untracked(ctx, key, repo)
	}
	// The IMPORT RULE of doc/graft/PLAN6.md: head sequence 0 is an import that
	// is not complete. The local repository may be the only copy of the objects
	// and references being imported, so it is never hydrated, caught up, moved
	// away or deleted in that state.
	if r.HeadSeq == 0 {
		return corestore.Repository{}, git.ObjectHash{}, structerr.NewFailedPrecondition("repository import is not complete; run ImportRepository again")
	}

	hash, err := git.ObjectHashByFormat(r.ObjectFormat)
	if err != nil {
		return corestore.Repository{}, git.ObjectHash{}, structerr.NewInternal("storage core: object format %q of repository %d: %w", r.ObjectFormat, r.ID, err)
	}
	return r, hash, nil
}

// untracked answers for a key the database does not have (the STALE CACHE RULE
// of doc/graft/PLAN6.md). The watermark proves that the storage core wrote a
// local repository (it was created, imported or hydrated here):
//
//   - no local repository: NotFound;
//   - a local repository with a watermark: the stale cache of a repository
//     removed (on any node). It is moved into the storage's temporary directory,
//     removed there in the background, and the answer is NotFound;
//   - a valid local repository without a (valid) watermark: it was never in the
//     storage core, FailedPrecondition "not imported", and it is left in place.
//
// The local repository is inspected before the database is read again, and a
// repository found then makes the sync start over (errRestart): CreateRepository
// and ImportRepository write the watermark only after the registration, so a
// repository registered meanwhile is never taken for a stale cache.
func (s *Syncer) untracked(ctx context.Context, key corestore.RepoKey, repo *gitalypb.Repository) error {
	path, err := s.locator.GetRepoPath(ctx, repo, storage.WithRepositoryVerificationSkipped())
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return structerr.NewNotFound("repository not found")
	} else if err != nil {
		return structerr.NewInternal("storage core: stat repository: %w", err)
	}

	// A corrupt watermark is no proof that the storage core wrote the
	// repository: it is treated as missing, so the repository is kept.
	wm, ok, err := gitops.ReadWatermark(path)
	if err != nil || !ok {
		if s.localValid(ctx, repo) {
			return structerr.NewFailedPrecondition("repository is not imported into the storage core")
		}
		return structerr.NewNotFound("repository not found")
	}

	if _, err := s.core.Meta.GetRepository(ctx, key); err == nil {
		return errRestart
	} else if !errors.Is(err, corestore.ErrRepositoryNotFound) {
		return structerr.NewUnavailable("storage core: get repository: %w", err)
	}

	if err := s.moveStale(ctx, repo, path, wm); err != nil {
		return err
	}
	return structerr.NewNotFound("repository not found")
}

// moveStale moves the stale cache at path, whose watermark was wm, into the
// temporary directory of the storage under the repository lock, and removes it
// there in the background (it is never deleted in place). It returns
// errRestart when the repository at path is no longer that cache: its
// watermark changed before the move, or the directory moved is another
// repository (one created at the same path in between, which is put back).
func (s *Syncer) moveStale(ctx context.Context, repo *gitalypb.Repository, path string, wm corestore.Watermark) error {
	tmpDir, err := s.tmpDir(repo.GetStorageName())
	if err != nil {
		return err
	}

	release, err := s.lock(ctx, path)
	if err != nil {
		return err
	}
	defer release()

	if current, ok, err := gitops.ReadWatermark(path); err != nil || !ok || current.RepoID != wm.RepoID {
		return errRestart
	}

	stale, err := os.MkdirTemp(tmpDir, tmpStalePattern)
	if err != nil {
		return structerr.NewInternal("storage core: create stale directory: %w", err)
	}
	moved := filepath.Join(stale, "repo.git")
	if err := os.Rename(path, moved); err != nil {
		_ = os.Remove(stale)
		if errors.Is(err, os.ErrNotExist) {
			// Moved away by someone else (the gate) meanwhile.
			return structerr.NewNotFound("repository not found")
		}
		return structerr.NewInternal("storage core: move stale repository: %w", err)
	}

	if current, ok, err := gitops.ReadWatermark(moved); err != nil || !ok || current.RepoID != wm.RepoID {
		// Not the cache that was checked: a repository was created at the path
		// between the check and the move. Put it back.
		if err := os.Rename(moved, path); err != nil {
			s.logger.WithError(err).WithField("repository_path", path).WithField("moved_path", moved).
				Error("storage core: could not put back a repository moved by mistake as a stale cache")
			return structerr.NewInternal("storage core: put back repository: %w", err)
		}
		_ = os.Remove(stale)
		return errRestart
	}

	s.forgetPending(path)
	s.logger.WithField("repository_path", path).WithField("stale_path", stale).WithField("repository_id", wm.RepoID).
		Info("storage core: moved away the stale cache of a removed repository")
	go func() { _ = os.RemoveAll(stale) }()
	return nil
}

// shadowHistoryError refuses to replace a local copy that holds history the
// database does not have yet (the message of the gate's guard).
func shadowHistoryError() structerr.Error {
	return structerr.NewFailedPrecondition("the shadow-mode history of this repository is not in the storage core yet: run graft storage import")
}

// checkReplaceable refuses (FailedPrecondition) to replace the local
// repository at path by a hydration while it holds history of the repository
// that the database does not have: file changelog entries beyond the
// database's log sequence, or file reviews that ImportHistory would still
// write (corestore.ReviewNeedsImport: created or changed in shadow mode). Shadow
// mode writes both into the local repository (doc/graft/PLAN6.md, LOCK AND
// HISTORY RULE); a files-backend copy imported in shadow mode is replaced by a
// reftable one in authoritative mode (REFTABLE RULE), and replacing it before
// ImportRepository brought that history in would delete it for good. The
// local repository is left in place; after the import the check passes.
//
// A local repository whose watermark names another repository ID is the cache
// of another repository: its history is not this repository's, and it is
// replaced. The repository lock is not held (the database is read); in
// authoritative mode nothing writes the file changelog or the file reviews.
func (s *Syncer) checkReplaceable(ctx context.Context, r corestore.Repository, path string, wm corestore.Watermark, hasWatermark bool) error {
	if hasWatermark && wm.RepoID != r.ID {
		return nil
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return structerr.NewInternal("storage core: stat repository: %w", err)
	}

	last, err := changelog.LastSequence(path)
	if err != nil {
		return structerr.NewInternal("storage core: reading the last sequence of the file changelog: %w", err)
	}
	if last > r.LogSeq {
		return shadowHistoryError().WithMetadata("file_changelog_sequence", last).WithMetadata("database_changelog_sequence", r.LogSeq)
	}

	fileReviews, err := reviewstore.Open(path).List("", true, 0)
	if err != nil {
		return structerr.NewInternal("storage core: reading the file reviews: %w", err)
	}
	if len(fileReviews) == 0 {
		return nil
	}
	dbReviews, err := s.core.Meta.ListReviews(ctx, r.ID, "", true, 0)
	if err != nil {
		return structerr.NewUnavailable("storage core: list reviews: %w", err)
	}
	byID := make(map[string]*gitalypb.Review, len(dbReviews))
	for _, review := range dbReviews {
		byID[review.GetId()] = review
	}
	for _, review := range fileReviews {
		if corestore.ReviewNeedsImport(review, byID[review.GetId()]) {
			return shadowHistoryError().WithMetadata("review_id", review.GetId())
		}
	}
	return nil
}

// localValid reports whether a valid local repository exists at the path of repo.
func (s *Syncer) localValid(ctx context.Context, repo storage.Repository) bool {
	path, err := s.locator.GetRepoPath(ctx, repo, storage.WithRepositoryVerificationSkipped())
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return s.locator.ValidateRepository(ctx, repo) == nil
}

// localCurrent reports whether the local repository at path (of repo) can be
// caught up: it is valid and uses the reftable reference backend. Any other
// local repository is a stale cache, replaced by a hydration like one of
// another repository ID (REFTABLE RULE: authoritative mode uses reftable
// repositories only, and hydration creates them).
func (s *Syncer) localCurrent(ctx context.Context, repo storage.Repository, path string) bool {
	if !s.localValid(ctx, repo) {
		return false
	}
	backend, err := gitcmd.DetectReferenceBackend(ctx, path)
	return err == nil && backend.Name == git.ReferenceBackendReftables.Name
}

// readWatermark returns the local watermark; a corrupt one is treated as
// absent, which makes the sync hydrate the repository.
func (s *Syncer) readWatermark(path string) (corestore.Watermark, bool) {
	w, ok, err := gitops.ReadWatermark(path)
	if err != nil {
		s.logger.WithError(err).WithField("repository_path", path).Warn("storage core: corrupt watermark, the repository is hydrated again")
		return corestore.Watermark{}, false
	}
	return w, ok
}

// acquireEntry returns the lock entry of path with one more reference.
func (s *Syncer) acquireEntry(path string) *repoLock {
	s.mu.Lock()
	defer s.mu.Unlock()

	l := s.locks[path]
	if l == nil {
		l = &repoLock{sem: make(chan struct{}, 1)}
		s.locks[path] = l
	}
	l.refs++
	return l
}

func (s *Syncer) releaseEntry(path string, l *repoLock) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l.refs--
	if l.refs == 0 {
		delete(s.locks, path)
	}
}

// lock waits for the repository lock of path until ctx is done. The lock is
// only ever held around local work, so the wait is short.
func (s *Syncer) lock(ctx context.Context, path string) (func(), error) {
	l := s.acquireEntry(path)
	select {
	case l.sem <- struct{}{}:
		return func() {
			<-l.sem
			s.releaseEntry(path, l)
		}, nil
	case <-ctx.Done():
		s.releaseEntry(path, l)
		return nil, structerr.NewUnavailable("storage core: waiting for the repository lock: %w", ctx.Err())
	}
}

// tryLock takes the repository lock of path without waiting.
func (s *Syncer) tryLock(path string) (func(), bool) {
	l := s.acquireEntry(path)
	select {
	case l.sem <- struct{}{}:
		return func() {
			<-l.sem
			s.releaseEntry(path, l)
		}, true
	default:
		s.releaseEntry(path, l)
		return nil, false
	}
}

// advance takes the repository lock and moves the watermark through the
// contiguous sequences noted by NoteCommitted. It returns errRestart when the
// watermark changed to another repository or disappeared meanwhile.
func (s *Syncer) advance(ctx context.Context, path string, wm corestore.Watermark) (corestore.Watermark, error) {
	release, err := s.lock(ctx, path)
	if err != nil {
		return wm, err
	}
	defer release()

	current, ok := s.readWatermark(path)
	if !ok || current.RepoID != wm.RepoID {
		return wm, errRestart
	}
	return s.advanceLocked(path, current), nil
}

// advanceLocked moves the watermark through the contiguous sequences noted by
// NoteCommitted. The caller holds the repository lock. On a write error the
// watermark stays where it was.
func (s *Syncer) advanceLocked(path string, wm corestore.Watermark) corestore.Watermark {
	s.mu.Lock()
	newSeq := wm.Seq
	if p := s.pending[path]; p != nil && p.repoID == wm.RepoID {
		for {
			if _, ok := p.seqs[newSeq+1]; !ok {
				break
			}
			newSeq++
		}
	}
	s.mu.Unlock()

	if newSeq == wm.Seq {
		s.dropPending(path, wm.RepoID, wm.Seq)
		return wm
	}

	next := corestore.Watermark{RepoID: wm.RepoID, Seq: newSeq}
	if err := gitops.WriteWatermark(path, next); err != nil {
		s.logger.WithError(err).WithField("repository_path", path).Error("storage core: write watermark")
		return wm
	}
	s.dropPending(path, wm.RepoID, newSeq)
	return next
}

// dropPending forgets the noted sequences up to upTo, and everything of another repository ID.
func (s *Syncer) dropPending(path string, repoID int64, upTo uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := s.pending[path]
	if p == nil {
		return
	}
	if p.repoID != repoID {
		delete(s.pending, path)
		return
	}
	for seq := range p.seqs {
		if seq <= upTo {
			delete(p.seqs, seq)
		}
	}
	if len(p.seqs) == 0 {
		delete(s.pending, path)
	}
}

// forgetPending forgets every noted sequence of path.
func (s *Syncer) forgetPending(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, path)
}

// report tells the database how far this node's cache is, at most once per
// reportInterval per repository. It is best effort: errors are only logged.
// It is never called with the repository lock held.
func (s *Syncer) report(ctx context.Context, r corestore.Repository, seq uint64) {
	s.mu.Lock()
	if time.Since(s.reported[r.ID]) < reportInterval {
		s.mu.Unlock()
		return
	}
	s.reported[r.ID] = time.Now()
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportInterval)
	defer cancel()
	if err := s.core.Meta.ReportApplied(ctx, r.ID, s.core.NodeID, seq); err != nil {
		s.logger.WithError(err).WithField("repository_id", r.ID).Warn("storage core: report applied sequence")
	}
}

// refHistory is what a range of changesets did to one reference.
type refHistory struct {
	// values are every value the reference had in the range, from the value
	// before the first changeset of the range to the final one ("" = absent).
	values map[corestore.Target]struct{}
	// final is the value after the last changeset of the range.
	final corestore.Target
}

// fetched is the result of downloading a range of changesets.
type fetched struct {
	// head is the last sequence of the range.
	head       uint64
	refs       map[string]*refHistory
	changesets int
	bytes      int64
}

func newFetched(after uint64) *fetched {
	return &fetched{head: after, refs: map[string]*refHistory{}}
}

// finalRefs returns the value of every changed reference after the range.
func (f *fetched) finalRefs() map[string]corestore.Target {
	refs := make(map[string]corestore.Target, len(f.refs))
	for name, h := range f.refs {
		refs[name] = h.final
	}
	return refs
}

// names returns the changed references.
func (f *fetched) names() []string {
	names := make([]string, 0, len(f.refs))
	for name := range f.refs {
		names = append(names, name)
	}
	return names
}

// catchUp applies the changesets (after, head] to the local repository at
// path. The packs are downloaded and indexed without the repository lock
// (adding objects never conflicts with a local write); the lock covers only the
// reference check, the reference transaction and the watermark. fresh is true
// when the watermark turned out to be at the head already when the lock was
// taken (the node committed the changesets itself meanwhile).
//
// Local writes run concurrently and never take the repository lock (git holds
// the reference locks, and the hooks must not wait for the syncer). So the
// apply is a compare-and-swap: each reference goes from the local value the
// check read to its final value, with that value (an object ID, absence, or a
// symbolic target) checked by git under the reference lock (applyMoves). A
// local write committed in between fails the transaction rather than being
// reverted, which matters because its NoteCommitted sequence would otherwise
// move the watermark over a reference that no longer has its value.
func (s *Syncer) catchUp(ctx context.Context, repo *gitalypb.Repository, r corestore.Repository, hash git.ObjectHash, path string, after uint64) (fresh bool, _ error) {
	tmpDir, err := s.tmpDir(repo.GetStorageName())
	if err != nil {
		return false, err
	}

	repoObj := s.repoFactory.Build(repo)
	f := newFetched(after)
	if err := s.fetch(ctx, repoObj, tmpDir, r.ID, hash, f, r.HeadSeq); err != nil {
		return false, err
	}

	s.hook("fetched")

	// repair is set once the database head was read again after a local value
	// that the range does not explain: the local reference is then simply wrong
	// and the database value is applied.
	repair := false
	backoff := initialBackoff
	for {
		chk, err := s.checkLocked(ctx, repoObj, path, r, f, repair)
		if err != nil {
			return false, err
		}
		if chk.done {
			return s.caughtUp(ctx, r, f, false), nil
		}

		if chk.mismatch == "" {
			// The check accepted every local value as one the range explains. A
			// value can also be the effect of a later changeset of this node that
			// happens to equal a value of the range (a branch reset to an earlier
			// commit): moving that reference would revert a committed write. Such
			// changesets are pulled into the range first.
			upTo, err := s.newerLocalWrites(ctx, r.ID, f.head, chk.moves)
			if err != nil {
				return false, err
			}
			if upTo > f.head {
				if err := s.fetch(ctx, repoObj, tmpDir, r.ID, hash, f, upTo); err != nil {
					return false, err
				}
				r.HeadSeq = max(r.HeadSeq, upTo)
				continue
			}

			s.hook("checked")

			out, err := s.applyLocked(ctx, repoObj, hash, path, r, f, chk.moves)
			if err != nil {
				return false, err
			}
			switch {
			case out.done:
				return s.caughtUp(ctx, r, f, out.applied), nil
			case out.changed:
				// A local write changed a reference since the check: the
				// transaction failed on its expected old value. Check again.
				continue
			}

			// A concurrent local writer holds a reference lock: back off and retry
			// (with the check again) until the sync budget runs out.
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return false, structerr.NewUnavailable("storage core: reference locked by a concurrent writer: %w", out.lockErr)
			case <-timer.C:
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// A local value that no changeset of the range explains: a local write
		// committed after the head was read (its changeset is in the database
		// already), or a damaged cache. Read the head again (without the lock)
		// and extend the range; if the head did not move, repair.
		latest, err := s.core.Meta.GetRepository(ctx, r.Key)
		if err != nil {
			if errors.Is(err, corestore.ErrRepositoryNotFound) {
				return false, structerr.NewNotFound("repository not found")
			}
			return false, structerr.NewUnavailable("storage core: get repository: %w", err)
		}
		if latest.ID != r.ID {
			return false, errRestart
		}
		if latest.HeadSeq > f.head {
			if err := s.fetch(ctx, repoObj, tmpDir, r.ID, hash, f, latest.HeadSeq); err != nil {
				return false, err
			}
			r.HeadSeq = latest.HeadSeq
			continue
		}
		s.logger.WithField("repository_path", path).WithField("reference", chk.mismatch).
			Warn("storage core: local reference diverged from the database, repairing it")
		repair = true
	}
}

// caughtUp records the metrics and the report of a finished catch-up and
// returns whether the local repository was fresh (applied false).
func (s *Syncer) caughtUp(ctx context.Context, r corestore.Repository, f *fetched, applied bool) bool {
	s.syncBytes.Add(float64(f.bytes))
	if applied {
		s.syncChangesets.Add(float64(f.changesets))
		s.report(ctx, r, f.head)
	}
	return !applied
}

// hook calls the test hook, if any.
func (s *Syncer) hook(point string) {
	if s.testHook != nil {
		s.testHook(point)
	}
}

// move is one reference of the range: from the local value the check read
// (and accepted) to the final value of the range. When both are equal it is a
// verification: git checks the value under the reference lock and nothing is
// written.
type move struct {
	old, new corestore.Target
}

// verify reports whether the move only verifies the reference.
func (m move) verify() bool {
	return m.old == m.new
}

// checkOutcome is the result of one checkLocked attempt.
type checkOutcome struct {
	// done reports that the watermark is at the head of the range already (the
	// node committed the changesets itself meanwhile).
	done bool
	// mismatch names a local reference whose value the range does not explain.
	mismatch string
	// moves are the references of the range, from the local values read (see
	// move).
	moves map[string]move
}

// checkLocked takes the repository lock and checks the local repository
// against the fetched range: the watermark must still belong to the
// repository (errRestart otherwise) and be behind the range, and every
// reference the range changed must have a local value the range explains
// (else it reports the mismatch, unless repair is set). It returns one move per
// reference of the range, from the local value read to the final one.
//
// A reference whose local value is final already gets a move with equal old
// and new values: a verification (casSteps), never a rewrite. A local write
// may move it right after the check, and writing the (older) final value over
// it would revert a committed change. It is still checked by git under the
// reference lock before the watermark claims the range: a write of this node
// whose changeset is in the range may be between git's prepared and committed
// states, with its reference locked and still showing the old value, which can
// equal the final value (another node set it back). Claiming the range without
// waiting for that lock would let the write's commit leave the reference at a
// value the watermark says was overwritten. The verification waits for the
// lock, then fails when the write committed, and the check runs again.
func (s *Syncer) checkLocked(ctx context.Context, repo *localrepo.Repo, path string, r corestore.Repository, f *fetched, repair bool) (checkOutcome, error) {
	release, err := s.lock(ctx, path)
	if err != nil {
		return checkOutcome{}, err
	}
	defer release()

	wm, ok := s.readWatermark(path)
	if !ok || wm.RepoID != r.ID {
		return checkOutcome{}, errRestart
	}
	wm = s.advanceLocked(path, wm)
	if wm.Seq >= f.head {
		return checkOutcome{done: true}, nil
	}

	local, err := gitops.ReadRefValues(ctx, repo, f.names())
	if err != nil {
		return checkOutcome{}, structerr.NewInternal("storage core: read local references: %w", err)
	}

	moves := map[string]move{}
	for name, h := range f.refs {
		current := local[name]
		if !repair {
			if _, known := h.values[current]; !known {
				return checkOutcome{mismatch: name}, nil
			}
		}
		moves[name] = move{old: current, new: h.final}
	}
	return checkOutcome{moves: moves}, nil
}

// newerLocalWrites looks at the changesets after the range (committed by now)
// for one made by this node that changed a reference that moves rewrite (the
// verifications write nothing and are left out). Its effect may be what the
// check read (a value that the range also contains), so the range must be
// extended through it before anything is moved. It returns the sequence to
// extend the range to, or after when there is no such changeset.
//
// Changesets of other nodes cannot be in the local repository (only this
// syncer applies them), and the changesets committed after this read only reach
// the local repository after the check, which the compare-and-swap of the apply
// detects. The repository lock is not held.
func (s *Syncer) newerLocalWrites(ctx context.Context, repoID int64, after uint64, moves map[string]move) (uint64, error) {
	upTo := after
	rewrites := 0
	for _, m := range moves {
		if !m.verify() {
			rewrites++
		}
	}
	if rewrites == 0 {
		return upTo, nil
	}

	for cur := after; ; {
		end := cur + changesetBatch
		if end < cur {
			end = math.MaxUint64
		}

		changesets, err := s.core.Meta.ListChangesets(ctx, repoID, cur, end)
		if err != nil {
			return 0, structerr.NewUnavailable("storage core: list changesets: %w", err)
		}

		for _, cs := range changesets {
			if cs.Seq <= cur {
				return 0, structerr.NewInternal("storage core: repository %d: changeset %d listed after %d", repoID, cs.Seq, cur)
			}
			cur = cs.Seq
			if cs.NodeID != s.core.NodeID {
				continue
			}
			for _, u := range cs.Updates {
				if m, ok := moves[u.Name]; ok && !m.verify() {
					upTo = cs.Seq
					break
				}
			}
		}

		if len(changesets) < changesetBatch {
			return upTo, nil
		}
	}
}

// applyOutcome is the result of one applyLocked attempt.
type applyOutcome struct {
	// done reports that the local repository is at the head of the range;
	// applied that this attempt moved the watermark there (false: it was there
	// already).
	done, applied bool
	// changed reports that a local reference no longer has the value the check
	// read: the check must run again.
	changed bool
	// lockErr is a reference lock held by a concurrent local writer (retryable).
	lockErr error
}

// applyLocked takes the repository lock and moves the references from the
// values the check read to their final values, with those values as the
// expected old values checked by git under its reference locks (applyMoves): a
// local write that committed in between makes the transaction fail instead of
// being overwritten. The references already final are verified the same way in
// the same transactions, so the transaction also waits for a local write that
// holds the lock between git's prepared and committed states. Then it writes
// the watermark. With no reference in the range at all it writes the watermark
// only.
func (s *Syncer) applyLocked(ctx context.Context, repo *localrepo.Repo, hash git.ObjectHash, path string, r corestore.Repository, f *fetched, moves map[string]move) (applyOutcome, error) {
	release, err := s.lock(ctx, path)
	if err != nil {
		return applyOutcome{}, err
	}
	defer release()

	wm, ok := s.readWatermark(path)
	if !ok || wm.RepoID != r.ID {
		return applyOutcome{}, errRestart
	}
	wm = s.advanceLocked(path, wm)
	if wm.Seq >= f.head {
		return applyOutcome{done: true}, nil
	}

	w := refWriter{core: s.core, repo: localrefs.RepoRef{Storage: r.Key.Storage, RelativePath: r.Key.RelativePath}, path: path}
	if err := applyMoves(ctx, repo, w, hash, moves); err != nil {
		var txErr *refTxError
		if errors.As(err, &txErr) && txErr.locked {
			return applyOutcome{lockErr: err}, nil
		}
		// git words a failed expected old value differently by command and
		// backend (is at X but expected Y, reference already exists, unable to
		// resolve, dangling symref already exists): the values are read again
		// instead of parsing the message. A failure that left every value as the
		// check read it is not a concurrent write, and is returned.
		if changed, readErr := movedSince(ctx, repo, moves); readErr == nil && changed {
			return applyOutcome{changed: true}, nil
		}
		return applyOutcome{}, structerr.NewInternal("storage core: apply references: %w", err)
	}

	if err := s.commitWatermarkLocked(path, r.ID, f.head); err != nil {
		return applyOutcome{}, err
	}
	return applyOutcome{done: true, applied: true}, nil
}

// commitWatermarkLocked writes the watermark (repoID, seq) and advances it
// through the sequences this node committed after seq, contiguous with it:
// their effects are in the local repository already. The caller holds the
// repository lock.
func (s *Syncer) commitWatermarkLocked(path string, repoID int64, seq uint64) error {
	wm := corestore.Watermark{RepoID: repoID, Seq: seq}
	if err := gitops.WriteWatermark(path, wm); err != nil {
		return structerr.NewInternal("storage core: write watermark: %w", err)
	}
	s.advanceLocked(path, wm)
	return nil
}

// movedSince reports whether a reference of moves no longer has its old value.
func movedSince(ctx context.Context, repo *localrepo.Repo, moves map[string]move) (bool, error) {
	names := make([]string, 0, len(moves))
	for name := range moves {
		names = append(names, name)
	}
	local, err := gitops.ReadRefValues(ctx, repo, names)
	if err != nil {
		return false, err
	}
	for name, m := range moves {
		if local[name] != m.old {
			return true, nil
		}
	}
	return false, nil
}

// applyMoves moves every reference of moves from its old value (the value the
// check read) to its new value, without hooks and without dereferencing, with
// the old value as the value git must find under its reference lock: an object
// ID, absence (the zero object ID), or the immediate target of a symbolic
// reference. A local write committed since the check therefore makes git fail
// the transaction, and nothing of that transaction is applied. A move with
// equal old and new values is only verified. This is the only compare-and-swap
// of the catch-up (casSteps lists the commands).
//
// The references other than HEAD are moved first, HEAD alone afterwards: git
// refuses one transaction that updates HEAD and the reference it points to.
func applyMoves(ctx context.Context, repo *localrepo.Repo, w refWriter, hash git.ObjectHash, moves map[string]move) error {
	var names []string
	moveHead := false
	for name, m := range moves {
		if err := gitops.ValidateName(name); err != nil {
			return fmt.Errorf("reference %q: %w", name, err)
		}
		for _, target := range []corestore.Target{m.old, m.new} {
			if err := gitops.ValidateTarget(target, hash); err != nil {
				return fmt.Errorf("reference %q: %w", name, err)
			}
		}
		if name == "HEAD" {
			moveHead = true
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	if len(names) > 0 {
		if err := applyChecked(ctx, repo, w, hash, names, moves); err != nil {
			return err
		}
	}
	if moveHead {
		return applyChecked(ctx, repo, w, hash, []string{"HEAD"}, moves)
	}
	return nil
}

// applyChecked applies the named moves in one checked transaction, or in two
// when a removal has to come first: the removals (deletions, and the first
// step of a symbolic reference that becomes an object ID), then the other
// changes. The split is needed for such a symbolic reference, which git cannot
// move in one step, and when git reports a file/directory conflict between a
// removed and a created name (refs/a and refs/a/b). Between the two
// transactions a removed reference is absent; the second transaction requires
// that, so a local write in between still fails it.
func applyChecked(ctx context.Context, repo *localrepo.Repo, w refWriter, hash git.ObjectHash, names []string, moves map[string]move) error {
	var removals, others []string
	twoSteps := false
	for _, name := range names {
		del, set := casSteps(name, moves[name], hash)
		if del != "" {
			removals = append(removals, del)
		}
		if set != "" {
			others = append(others, set)
		}
		if del != "" && set != "" {
			twoSteps = true
		}
	}

	if !twoSteps {
		err := refTransaction(ctx, repo, w, append(append([]string{}, removals...), others...))
		var txErr *refTxError
		if err == nil || !errors.As(err, &txErr) || !txErr.fdConflict || len(removals) == 0 || len(others) == 0 {
			return err
		}
	}

	if err := refTransaction(ctx, repo, w, removals); err != nil {
		return fmt.Errorf("apply removals: %w", err)
	}
	return refTransaction(ctx, repo, w, others)
}

// refWriter runs the reference transactions of the catch-up on the local
// repository at path, under the syncer's repository lock: admitted to git's
// reference lock like the hook's writers, and without fsync when the node
// defers the durability of its reference writes (doc/graft/PLAN7.md, LOCAL
// REFERENCE WRITES RULE).
type refWriter struct {
	core *corestore.Core
	repo localrefs.RepoRef
	path string
}

// nul terminates the fields of update-ref -z.
const nul = "\x00"

// casSteps returns the `update-ref -z --stdin` commands of a move, each with
// the old value checked: del removes the reference, set gives it its new
// value. A move has one of them, or both when a symbolic reference becomes an
// object ID: git has no command for that with the symbolic value checked, so
// the reference is removed with its target checked (symref-delete), then
// created with the zero object ID as the old value (it must be absent). A
// verification (equal old and new values) is a verify or symref-verify
// command in set: git checks the value under the reference lock and writes
// nothing.
func casSteps(name string, m move, hash git.ObjectHash) (del, set string) {
	oldOID := hash.ZeroOID.String()
	if !m.old.Absent() && !m.old.IsSymbolic() {
		oldOID = string(m.old)
	}

	switch {
	case m.verify() && m.old.IsSymbolic():
		return "", "symref-verify " + name + nul + m.old.SymbolicName() + nul
	case m.verify():
		return "", "verify " + name + nul + oldOID + nul
	case m.new.Absent() && m.old.IsSymbolic():
		return "symref-delete " + name + nul + m.old.SymbolicName() + nul, ""
	case m.new.Absent():
		return "delete " + name + nul + oldOID + nul, ""
	case m.new.IsSymbolic() && m.old.IsSymbolic():
		return "", "symref-update " + name + nul + m.new.SymbolicName() + nul + "ref" + nul + m.old.SymbolicName() + nul
	case m.new.IsSymbolic():
		return "", "symref-update " + name + nul + m.new.SymbolicName() + nul + "oid" + nul + oldOID + nul
	case m.old.IsSymbolic():
		return "symref-delete " + name + nul + m.old.SymbolicName() + nul,
			"update " + name + nul + string(m.new) + nul + hash.ZeroOID.String() + nul
	default:
		return "", "update " + name + nul + string(m.new) + nul + oldOID + nul
	}
}

// refTxError is a failed transaction of refTransaction, classified with the
// error messages of the repository's reference backend.
type refTxError struct {
	err    error
	stderr string
	// locked reports that a concurrent writer held a reference lock (retryable).
	locked bool
	// fdConflict reports a file/directory conflict between reference names.
	fdConflict bool
}

func (e *refTxError) Error() string {
	return fmt.Sprintf("update-ref: %v: %s", e.err, strings.TrimSpace(e.stderr))
}

func (e *refTxError) Unwrap() error { return e.err }

// refTransaction runs the commands (casSteps) as one transaction of
// `git update-ref -z --stdin --no-deref`, without hooks (the database has the
// changes already: no reference-transaction hook may run for them). An empty
// command list does nothing. With deferred durability the repository gets its
// dirty marker before git writes; the caller holds the syncer's repository
// lock, which the durable step holds too.
func refTransaction(ctx context.Context, repo *localrepo.Repo, w refWriter, commands []string) error {
	if len(commands) == 0 {
		return nil
	}

	backend, err := repo.ReferenceBackend(ctx)
	if err != nil {
		return fmt.Errorf("reference backend: %w", err)
	}

	// Only while this process owns the deferred durability; the syncer's
	// repository lock is held, and the ownership never goes back.
	var durability []gitcmd.CmdOpt
	if w.core.RefDurability.Deferring() {
		if err := w.core.RefDurability.MarkDirty(ctx, w.repo, w.path); err != nil {
			return err
		}
		durability = append(durability, gitcmd.WithConfig(gitcmd.ConfigPair{Key: "core.fsync", Value: gitcmd.FsyncComponentsWithoutReferences}))
	}
	if release := w.core.RefAdmission.AcquireUnique(ctx, w.path, localrefs.WriterSyncer); release != nil {
		defer release()
	}

	var stdin bytes.Buffer
	stdin.WriteString("start" + nul)
	for _, c := range commands {
		stdin.WriteString(c)
	}
	stdin.WriteString("commit" + nul)

	var stderr bytes.Buffer
	err = repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "update-ref",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "-z"},
				gitcmd.Flag{Name: "--stdin"},
				gitcmd.Flag{Name: "--no-deref"},
			},
		},
		append([]gitcmd.CmdOpt{
			gitcmd.WithDisabledHooks(),
			gitcmd.WithStdin(&stdin),
			gitcmd.WithStdout(io.Discard),
			gitcmd.WithStderr(&stderr),
		}, durability...)...,
	)
	if err == nil {
		return nil
	}

	msg := stderr.Bytes()
	matches := func(re *regexp.Regexp) bool {
		// The backends' regular expressions have groups; an empty expression
		// (no such error for the backend) matches without any.
		return re != nil && len(re.FindSubmatch(msg)) > 1
	}
	return &refTxError{
		err:        err,
		stderr:     stderr.String(),
		locked:     matches(backend.RefLockedRegex) || matches(backend.PackedRefsLockedRegex),
		fdConflict: matches(backend.ReferenceExistsConflictRegex) || matches(backend.InTransactionConflictRegex),
	}
}

// hydrate builds a bare repository in the temporary directory of the storage,
// then moves it to path. It starts from the newest checkpoint when there is one
// (its pack and its reference snapshot) and replays the changesets after it,
// otherwise from changeset 1 (doc/graft/PLAN7.md, "Hydration from a
// checkpoint"). A local repository at path (another repository ID, invalid, or
// below boundary, C_sync) is moved away first and removed in the background.
// Only the final moves are done under the repository lock.
func (s *Syncer) hydrate(ctx context.Context, repo *gitalypb.Repository, r corestore.Repository, hash git.ObjectHash, path string, boundary uint64) error {
	storageName := repo.GetStorageName()

	storagePath, err := s.locator.GetStorageByName(ctx, storageName)
	if err != nil {
		return err
	}
	tmpDir, err := s.tmpDir(storageName)
	if err != nil {
		return err
	}

	build, err := os.MkdirTemp(tmpDir, tmpHydratePatt)
	if err != nil {
		return structerr.NewInternal("storage core: create hydration directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(build) }()

	buildPath := filepath.Join(build, "repo.git")
	if err := gitops.InitBare(ctx, s.gitCmdFactory, buildPath, r.ObjectFormat); err != nil {
		return structerr.NewInternal("storage core: init repository: %w", err)
	}

	rel, err := filepath.Rel(storagePath, buildPath)
	if err != nil {
		return structerr.NewInternal("storage core: relative hydration path: %w", err)
	}
	buildRepo := s.repoFactory.Build(&gitalypb.Repository{StorageName: storageName, RelativePath: rel})

	refs, f, err := s.seed(ctx, buildRepo, tmpDir, r, hash)
	if err != nil {
		return err
	}
	if err := s.fetch(ctx, buildRepo, tmpDir, r.ID, hash, f, r.HeadSeq); err != nil {
		return err
	}
	for name, target := range f.finalRefs() {
		if target.Absent() {
			delete(refs, name)
		} else {
			refs[name] = target
		}
	}
	if err := s.applyBuild(ctx, buildRepo, refs); err != nil {
		return err
	}
	if err := gitops.WriteWatermark(buildPath, corestore.Watermark{RepoID: r.ID, Seq: f.head}); err != nil {
		return structerr.NewInternal("storage core: write watermark: %w", err)
	}

	if err := s.install(ctx, repo, r, path, buildPath, tmpDir, boundary); err != nil {
		return err
	}

	s.syncChangesets.Add(float64(f.changesets))
	s.syncBytes.Add(float64(f.bytes))
	s.report(ctx, r, f.head)
	return nil
}

// seed starts a hydration: from the newest checkpoint when there is one (its
// pack, verified against its key and indexed into repo, and its validated
// reference snapshot), else from nothing. It returns the references to start
// from and the fetch state positioned after the seed.
func (s *Syncer) seed(ctx context.Context, repo *localrepo.Repo, tmpDir string, r corestore.Repository, hash git.ObjectHash) (map[string]corestore.Target, *fetched, error) {
	cp, ok, err := s.core.Meta.NewestCheckpoint(ctx, r.ID)
	if err != nil {
		return nil, nil, structerr.NewUnavailable("storage core: newest checkpoint: %w", err)
	}
	if !ok {
		return map[string]corestore.Target{}, newFetched(0), nil
	}

	// Data read from the database is validated before it touches the
	// repository (RULE 22).
	if id, _, ok := corestore.ParsePackKey(cp.PackKey); !ok || id != r.ID {
		return nil, nil, structerr.NewInternal("storage core: checkpoint %d has an invalid pack key", cp.Seq)
	}
	refs, err := s.core.Meta.CheckpointRefs(ctx, r.ID, cp.Seq)
	if err != nil {
		return nil, nil, structerr.NewUnavailable("storage core: checkpoint %d references: %w", cp.Seq, err)
	}
	for name, target := range refs {
		if target.Absent() {
			return nil, nil, structerr.NewInternal("storage core: checkpoint %d: reference %q has no value", cp.Seq, name)
		}
		if err := gitops.ValidateName(name); err != nil {
			return nil, nil, structerr.NewInternal("storage core: checkpoint %d: invalid reference name %q: %w", cp.Seq, name, err)
		}
		if err := gitops.ValidateTarget(target, hash); err != nil {
			return nil, nil, structerr.NewInternal("storage core: checkpoint %d: invalid target of %q: %w", cp.Seq, name, err)
		}
		if err := validateSymbolicTarget(name, target); err != nil {
			return nil, nil, structerr.NewInternal("storage core: checkpoint %d: invalid target of %q: %w", cp.Seq, name, err)
		}
	}

	f := newFetched(cp.Seq)
	n, err := s.indexPack(ctx, repo, tmpDir, cp.PackKey)
	if err != nil {
		return nil, nil, err
	}
	f.bytes += n
	s.syncSeeds.Inc()
	return refs, f, nil
}

// install moves the hydrated repository at buildPath to path under the
// repository lock, moving a local repository that is in the way into the
// temporary directory first. It returns errRestart when, meanwhile, the local
// repository became one that a catch-up can bring up to date (a reftable
// repository with a watermark of the repository at or above boundary, C_sync:
// it was imported, created or caught up here): it is caught up instead of
// being replaced. The condition is the negation of the one that made sync
// hydrate, so a repository that is still stale (for example a files-backend
// one with a matching watermark, or one below C_sync) is replaced, never sent
// back to sync.
func (s *Syncer) install(ctx context.Context, repo *gitalypb.Repository, r corestore.Repository, path, buildPath, tmpDir string, boundary uint64) error {
	release, err := s.lock(ctx, path)
	if err != nil {
		return err
	}
	defer release()

	if wm, ok := s.readWatermark(path); ok && wm.RepoID == r.ID && wm.Seq >= boundary && s.localCurrent(ctx, repo, path) {
		return errRestart
	}

	if _, err := os.Lstat(path); err == nil {
		stale, err := os.MkdirTemp(tmpDir, tmpStalePattern)
		if err != nil {
			return structerr.NewInternal("storage core: create stale directory: %w", err)
		}
		if err := os.Rename(path, filepath.Join(stale, "repo.git")); err != nil {
			_ = os.Remove(stale)
			return structerr.NewInternal("storage core: move stale repository: %w", err)
		}
		s.logger.WithField("repository_path", path).WithField("stale_path", stale).
			Info("storage core: moved a stale local repository away before hydration")
		go func() { _ = os.RemoveAll(stale) }()
	} else if !errors.Is(err, os.ErrNotExist) {
		return structerr.NewInternal("storage core: stat repository: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return structerr.NewInternal("storage core: create repository parent: %w", err)
	}

	// Sequences noted for the replaced repository describe writes that happened
	// in it, not in the hydrated one: they are forgotten (the next sync fetches
	// them from the database) right before the hydrated repository is renamed
	// into place, so that no note of a write to the hydrated repository is lost.
	// The repository lock is held, so no advance runs in between.
	s.forgetPending(path)
	if err := os.Rename(buildPath, path); err != nil {
		return structerr.NewInternal("storage core: install hydrated repository: %w", err)
	}
	return nil
}

// tmpDir returns the temporary directory of the storage, created when needed.
func (s *Syncer) tmpDir(storageName string) (string, error) {
	dir, err := s.locator.TempDir(storageName)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", structerr.NewInternal("storage core: create temporary directory: %w", err)
	}
	return dir, nil
}

// fetch reads the changesets (f.head, upTo] from the database in batches,
// validates them, downloads and indexes their packs into repo and records in f
// what they did to every reference. Nothing is applied to the references
// here. Each batch is validated before its packs are downloaded. The
// repository lock is not held.
func (s *Syncer) fetch(ctx context.Context, repo *localrepo.Repo, tmpDir string, repoID int64, hash git.ObjectHash, f *fetched, upTo uint64) error {
	for f.head < upTo {
		cur := f.head
		end := cur + changesetBatch
		if end > upTo || end < cur {
			end = upTo
		}

		changesets, err := s.core.Meta.ListChangesets(ctx, repoID, cur, end)
		if err != nil {
			return structerr.NewUnavailable("storage core: list changesets: %w", err)
		}
		if uint64(len(changesets)) != end-cur {
			return structerr.NewInternal("storage core: repository %d: expected changesets %d to %d, the database returned %d", repoID, cur+1, end, len(changesets))
		}

		for i, cs := range changesets {
			if err := validateChangeset(cs, repoID, cur+uint64(i)+1, hash); err != nil {
				return err
			}
		}

		for _, cs := range changesets {
			if cs.PackKey != "" {
				n, err := s.indexPack(ctx, repo, tmpDir, cs.PackKey)
				if err != nil {
					return err
				}
				f.bytes += n
			}
			for _, u := range cs.Updates {
				h := f.refs[u.Name]
				if h == nil {
					h = &refHistory{values: map[corestore.Target]struct{}{u.Old: {}}}
					f.refs[u.Name] = h
				}
				h.values[u.New] = struct{}{}
				h.final = u.New
			}
		}

		f.changesets += len(changesets)
		f.head = end
	}

	return nil
}

// validateChangeset checks a changeset read from the database (RULE 10): its
// sequence, its pack key and every reference name and new target.
func validateChangeset(cs corestore.Changeset, repoID int64, wantSeq uint64, hash git.ObjectHash) error {
	if cs.RepoID != repoID || cs.Seq != wantSeq {
		return structerr.NewInternal("storage core: repository %d: got changeset %d of repository %d, want changeset %d", repoID, cs.Seq, cs.RepoID, wantSeq)
	}
	if cs.PackKey != "" {
		if id, _, ok := corestore.ParsePackKey(cs.PackKey); !ok || id != repoID {
			return structerr.NewInternal("storage core: changeset %d has an invalid pack key", cs.Seq)
		}
	}
	for _, u := range cs.Updates {
		if err := gitops.ValidateName(u.Name); err != nil {
			return structerr.NewInternal("storage core: changeset %d: invalid reference name %q: %w", cs.Seq, u.Name, err)
		}
		if err := gitops.ValidateTarget(u.New, hash); err != nil {
			return structerr.NewInternal("storage core: changeset %d: invalid target of %q: %w", cs.Seq, u.Name, err)
		}
		if err := validateSymbolicTarget(u.Name, u.New); err != nil {
			return structerr.NewInternal("storage core: changeset %d: invalid target of %q: %w", cs.Seq, u.Name, err)
		}
	}
	return nil
}

// validateSymbolicTarget rejects the symbolic targets that git refuses: one
// naming the reference itself (gitops.ValidateTarget cannot see the name), and
// one naming HEAD (gitops.ValidateTarget rejects it too; checked again here as
// defence in depth). Rejecting them before anything is applied keeps a
// catch-up from failing halfway, after the plain references were moved.
func validateSymbolicTarget(name string, t corestore.Target) error {
	if !t.IsSymbolic() {
		return nil
	}
	switch t.SymbolicName() {
	case "HEAD":
		return errors.New("a symbolic reference cannot point to HEAD")
	case name:
		return errors.New("a symbolic reference cannot point to itself")
	}
	return nil
}

// indexPack downloads the pack of key into a temporary file while hashing it,
// checks its SHA-256 against the key, and indexes it into repo. It returns the
// size of the pack.
func (s *Syncer) indexPack(ctx context.Context, repo *localrepo.Repo, tmpDir, key string) (int64, error) {
	_, want, ok := corestore.ParsePackKey(key)
	if !ok {
		return 0, structerr.NewInternal("storage core: invalid pack key %q", key)
	}

	rc, err := s.core.Blobs.Get(ctx, key)
	if err != nil {
		if errors.Is(err, corestore.ErrBlobNotFound) {
			return 0, structerr.NewInternal("storage core: pack %q is missing from the bucket: %w", key, err)
		}
		return 0, structerr.NewUnavailable("storage core: get pack %q: %w", key, err)
	}
	defer func() { _ = rc.Close() }()

	f, err := os.CreateTemp(tmpDir, tmpPackPattern)
	if err != nil {
		return 0, structerr.NewInternal("storage core: create pack file: %w", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()

	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hasher), rc)
	if err != nil {
		if errors.Is(err, corestore.ErrBlobCorrupt) {
			return 0, structerr.NewInternal("storage core: pack %q: %w", key, err)
		}
		return 0, structerr.NewUnavailable("storage core: download pack %q: %w", key, err)
	}
	var got [32]byte
	copy(got[:], hasher.Sum(nil))
	if got != want {
		return 0, structerr.NewInternal("storage core: pack %q: %w", key, corestore.ErrBlobCorrupt)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, structerr.NewInternal("storage core: rewind pack file: %w", err)
	}
	if err := gitops.IndexPack(ctx, repo, f); err != nil {
		return 0, structerr.NewInternal("storage core: index pack %q: %w", key, err)
	}
	return n, nil
}

// applyBuild sets the references of the repository being hydrated. Nothing
// else knows that repository, so a lock error is not expected; it is retried
// like any other all the same.
func (s *Syncer) applyBuild(ctx context.Context, repo *localrepo.Repo, refs map[string]corestore.Target) error {
	if len(refs) == 0 {
		return nil
	}

	backoff := initialBackoff
	for {
		err := gitops.ApplyRefs(ctx, repo, refs)
		if err == nil {
			return nil
		}
		if !gitops.IsLockError(err) {
			return structerr.NewInternal("storage core: apply references: %w", err)
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return structerr.NewUnavailable("storage core: reference locked: %w", err)
		case <-timer.C:
		}
		backoff = min(backoff*2, maxBackoff)
	}
}
