package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/storagemgr/partition/migration"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const backupIDPrefix = "backup_ids"

var (
	// ErrSkipped means the repository was skipped because there was nothing to backup
	ErrSkipped = errors.New("repository skipped")
	// ErrDoesntExist means that the data was not found.
	ErrDoesntExist = errors.New("doesn't exist")
	// ErrInvalidPathPrefix means the backup path prefix is invalid
	ErrInvalidPathPrefix = errors.New("invalid backup path prefix")

	backupLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "gitaly_backup_latency_seconds",
			Help: "Latency of a repository backup by phase",
		},
		[]string{"phase"})
	backupBundleSize = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "gitaly_backup_bundle_bytes",
			Help:    "Size of a Git bundle uploaded in a backup",
			Buckets: prometheus.ExponentialBucketsRange(1, 10*math.Pow(1024, 3), 20), // up to 10GB
		})
)

// Backup represents all the information needed to restore a backup for a repository
type Backup struct {
	// ID is the identifier that uniquely identifies the backup for this repository.
	ID string `toml:"-"`
	// Repository is the repository being backed up.
	Repository storage.Repository `toml:"-"`
	// Empty is true if the repository is empty
	Empty bool `toml:"empty"`
	// NonExistent is true if the repository does not exist.
	// Any Project/Non-Project that doesn't have any repository initialised can be considered as non-existent.
	NonExistent bool `toml:"non_existent"`
	// Steps are the ordered list of steps required to restore this backup
	Steps []Step `toml:"steps"`
	// ObjectFormat is the name of the object hash used by the repository.
	ObjectFormat string `toml:"object_format"`
	// HeadReference is the reference that HEAD points to.
	HeadReference string `toml:"head_reference,omitempty"`
}

// Step represents an incremental step that makes up a complete backup for a repository
type Step struct {
	// BundlePath is the path of the bundle
	BundlePath string `toml:"bundle_path,omitempty"`
	// RefPath is the path of the ref file
	RefPath string `toml:"ref_path,omitempty"`
	// PreviousRefPath is the path of the previous ref file
	PreviousRefPath string `toml:"previous_ref_path,omitempty"`
	// CustomHooksPath is the path of the custom hooks archive
	CustomHooksPath string `toml:"custom_hooks_path,omitempty"`
}

// locatorConfig holds optional configuration for the Locator.
type locatorConfig struct {
	pathPrefix     string
	latestBackupID string
}

// Option configures optional Locator behaviour.
type Option func(*locatorConfig)

// WithPathPrefix sets a path prefix that is prepended to all backup paths.
func WithPathPrefix(prefix string) Option {
	return func(cfg *locatorConfig) {
		cfg.pathPrefix = prefix
	}
}

// WithLatestBackupID sets a the latest backup ID to be used by incremental backups.
func WithLatestBackupID(latestID string) Option {
	return func(cfg *locatorConfig) {
		cfg.latestBackupID = latestID
	}
}

func applyOptions(opts []Option) locatorConfig {
	var cfg locatorConfig
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

// Locator finds sink backup paths for repositories
type Locator interface {
	// BeginFull returns a backup object with the tentative backup paths needed to create a full backup.
	BeginFull(ctx context.Context, repo storage.Repository, backupID string, opts ...Option) *Backup

	// BeginIncremental returns the backup with the last element of Steps being
	// the tentative step needed to create an incremental backup.
	BeginIncremental(ctx context.Context, repo storage.Repository, backupID string, opts ...Option) (*Backup, error)

	// Commit persists the backup so that it can be looked up by FindLatest. It
	// is expected that the last element of Steps will be the newly created
	// backup.
	Commit(ctx context.Context, backup *Backup, opts ...Option) error

	// FindLatest returns the latest backup that was written by Commit
	FindLatest(ctx context.Context, repo storage.Repository, opts ...Option) (*Backup, error)

	// Find returns the repository backup at the given backupID. If the backup does
	// not exist then the error ErrDoesntExist is returned.
	Find(ctx context.Context, repo storage.Repository, backupID string, opts ...Option) (*Backup, error)
}

// Repository abstracts git access required to make a repository backup
type Repository interface {
	// ListRefs returns an iterator to fetch the full set of refs and targets for the repository.
	ListRefs(ctx context.Context) (RefIterator, error)
	// GetCustomHooks fetches the custom hooks archive.
	GetCustomHooks(ctx context.Context, out io.Writer) error
	// CreateBundle fetches a bundle that contains refs matching patterns. When
	// patterns is nil all refs are bundled.
	CreateBundle(ctx context.Context, out io.Writer, patterns io.Reader) error
	// Remove removes the repository. Does not return an error if the
	// repository cannot be found.
	Remove(ctx context.Context) error
	// Create creates the repository. A zero refBackend leaves the backend to the node.
	Create(ctx context.Context, hash git.ObjectHash, refBackend git.ReferenceBackend, defaultBranch string) error
	// FetchBundle fetches references from a bundle. Refs will be mirrored to
	// the repository.
	FetchBundle(ctx context.Context, reader io.Reader, updateHead bool) error
	// SetCustomHooks updates the custom hooks for the repository.
	SetCustomHooks(ctx context.Context, reader io.Reader) error
	// ObjectHash detects the object hash used by the repository.
	ObjectHash(ctx context.Context) (git.ObjectHash, error)
	// HeadReference fetches the reference pointed to by HEAD.
	HeadReference(ctx context.Context) (git.ReferenceName, error)
	// ResetRefs attempts to reset the list of refs in the repository to match the
	// specified refs slice. This can fail if objects pointed to by a ref no longer
	// exists in the repository. The list of refs should not include the symbolic
	// HEAD reference.
	// If optimistic, update ref failures are ignored for regular updates but not
	// for removal updates.
	ResetRefs(ctx context.Context, refs []git.Reference, optimistic bool) error
	// SetHeadReference sets the symbolic HEAD reference of the repository to the
	// given target, for example a branch name.
	SetHeadReference(ctx context.Context, target git.ReferenceName) error
}

// ValidatePathPrefix validates that the backup path prefix is safe for use in path construction.
// It rejects absolute paths and paths containing ".." to prevent directory traversal attacks.
// Returns ErrInvalidPathPrefix if validation fails.
func ValidatePathPrefix(prefix string) error {
	if prefix == "" {
		// Empty prefix is allowed
		return nil
	}

	if filepath.IsAbs(prefix) {
		return fmt.Errorf("%w: must be a relative path", ErrInvalidPathPrefix)
	}

	for _, part := range strings.Split(prefix, "/") {
		if part == "" || strings.Trim(part, ".") == "" {
			return fmt.Errorf("%w: must be a clean relative path without traversal", ErrInvalidPathPrefix)
		}
	}

	return nil
}

// IDManager handles run-level backup ID operations against a backup sink.
// It is safe to use independently of Manager.
type IDManager struct {
	sink *Sink
}

// NewIDManager creates a BackupIDManager for the given sink.
func NewIDManager(sink *Sink) *IDManager {
	return &IDManager{sink: sink}
}

// Manager manages process of the creating/restoring backups.
type Manager struct {
	IDManager
	locator Locator
	logger  log.Logger

	// repositoryFactory returns an abstraction over git repositories in order
	// to create and restore backups.
	repositoryFactory func(ctx context.Context, repo *gitalypb.Repository, server storage.ServerInfo) (Repository, error)
}

// NewManager creates and returns initialized *Manager instance.
func NewManager(sink *Sink, logger log.Logger, locator Locator, pool *client.Pool) *Manager {
	return &Manager{
		IDManager: IDManager{sink: sink},
		locator:   locator,
		repositoryFactory: func(ctx context.Context, repo *gitalypb.Repository, server storage.ServerInfo) (Repository, error) {
			if err := setContextServerInfo(ctx, &server, repo.GetStorageName()); err != nil {
				return nil, err
			}

			conn, err := pool.Dial(ctx, server.Address, server.Token)
			if err != nil {
				return nil, err
			}

			return NewRemoteRepository(repo, conn), nil
		},
		logger: logger,
	}
}

// NewManagerLocal creates and returns a *Manager instance for operating on local repositories.
func NewManagerLocal(
	sink *Sink,
	logger log.Logger,
	locator Locator,
	storageLocator storage.Locator,
	gitCmdFactory gitcmd.CommandFactory,
	catfileCache catfile.Cache,
	txManager transaction.Manager,
	repoCounter *counter.RepositoryCounter,
	migrationStateManager migration.StateManager,
) *Manager {
	return &Manager{
		IDManager: IDManager{sink: sink},
		locator:   locator,
		repositoryFactory: func(ctx context.Context, repo *gitalypb.Repository, server storage.ServerInfo) (Repository, error) {
			localRepo := localrepo.New(logger, storageLocator, gitCmdFactory, catfileCache, repo)

			return NewLocalRepository(
				logger,
				storageLocator,
				gitCmdFactory,
				txManager,
				repoCounter,
				catfileCache,
				localRepo,
				migrationStateManager,
			), nil
		},
		logger: logger,
	}
}

// streamRefs streams the full set of refs into the backup sink.
func (mgr *Manager) streamRefs(ctx context.Context, repo Repository, path string) (refCount int, returnErr error) {
	timer := prometheus.NewTimer(backupLatency.WithLabelValues("refs"))
	defer timer.ObserveDuration()

	w, err := mgr.sink.GetWriter(ctx, path)
	if err != nil {
		return refCount, fmt.Errorf("stream refs: get writer: %w", err)
	}
	defer func() {
		if err := w.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("stream refs: close writer: %w", err))
		}
	}()

	iterator, err := repo.ListRefs(ctx)
	if err != nil {
		return refCount, fmt.Errorf("stream refs: %w", err)
	}
	defer func() {
		if err := iterator.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("stream refs: close iterator: %w", err))
		}
	}()

	for iterator.Next() {
		ref := iterator.Ref()
		_, err = fmt.Fprintf(w, "%s %s\n", ref.Target, ref.Name)
		if err != nil {
			return refCount, fmt.Errorf("stream refs: write ref: %w", err)
		}

		refCount++
	}
	if err := iterator.Err(); err != nil && !errors.Is(err, io.EOF) {
		return refCount, fmt.Errorf("stream refs: ref iterator: %w", err)
	}

	return refCount, nil
}

// Create creates a repository backup.
func (mgr *Manager) Create(ctx context.Context, req *CreateRequest) error {
	if req.VanityRepository == nil {
		req.VanityRepository = req.Repository
	}
	repo, err := mgr.repositoryFactory(ctx, req.Repository, req.Server)
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	var locatorOpts []Option
	if req.PathPrefix != "" {
		locatorOpts = append(locatorOpts, WithPathPrefix(req.PathPrefix))
	}
	var backup *Backup
	if req.Incremental {
		if req.LatestBackupID != "" {
			locatorOpts = append(locatorOpts, WithLatestBackupID(req.LatestBackupID))
		}
		backup, err = mgr.locator.BeginIncremental(ctx, req.VanityRepository, req.BackupID, locatorOpts...)
		if err != nil {
			return fmt.Errorf("manager: %w", err)
		}
	} else {
		backup = mgr.locator.BeginFull(ctx, req.VanityRepository, req.BackupID, locatorOpts...)
	}

	hash, err := repo.ObjectHash(ctx)
	switch {
	case status.Code(err) == codes.NotFound:
		backup.NonExistent = true
		backup.Empty = true
		return mgr.locator.Commit(ctx, backup, locatorOpts...)

	case err != nil:
		return fmt.Errorf("manager: %w", err)
	}

	backup.ObjectFormat = hash.Format

	headRef, err := repo.HeadReference(ctx)
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	backup.HeadReference = headRef.String()

	step := &backup.Steps[len(backup.Steps)-1]
	refCount, err := mgr.streamRefs(ctx, repo, step.RefPath)
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	if refCount > 0 {
		if err := mgr.writeBundle(ctx, repo, step); err != nil {
			return fmt.Errorf("manager: %w", err)
		}
	} else {
		backup.Empty = true
	}

	if err := mgr.writeCustomHooks(ctx, repo, step.CustomHooksPath); err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	if err := mgr.locator.Commit(ctx, backup, locatorOpts...); err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	return nil
}

// Restore restores a repository from a backup. If req.BackupID is empty, the
// latest backup will be used.
func (mgr *Manager) Restore(ctx context.Context, req *RestoreRequest) error {
	if req.VanityRepository == nil {
		req.VanityRepository = req.Repository
	}

	repo, err := mgr.repositoryFactory(ctx, req.Repository, req.Server)
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	var locatorOpts []Option
	if req.PathPrefix != "" {
		locatorOpts = append(locatorOpts, WithPathPrefix(req.PathPrefix))
	}
	var backup *Backup
	if req.BackupID != "" {
		backup, err = mgr.locator.Find(ctx, req.VanityRepository, req.BackupID, locatorOpts...)
	}
	// Fall back to per-repo latest when no backup ID was provided (e.g.
	// backup_ids/ folder not yet populated after upgrade) or when the
	// specific backup ID doesn't exist for this repo (partial backup run).
	if req.BackupID == "" || (errors.Is(err, ErrDoesntExist) && req.UseLatest) {
		backup, err = mgr.locator.FindLatest(ctx, req.VanityRepository, locatorOpts...)
		if errors.Is(err, ErrDoesntExist) {
			return removeRepository(ctx, repo, err)
		}
	}
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	if len(backup.Steps) == 0 {
		return fmt.Errorf("manager: no backup steps")
	}

	// The requested backend only applies if this restore creates the repository. An existing
	// repository keeps its own, because only the binary serving that backend can read it. We
	// keep it by leaving the context alone, since the MVCC middleware already set it.
	//
	// An empty backup is the exception: it recreates the repository, so the request decides.
	//
	// Where the backend applies, it stays on the context for the whole restore. The middleware
	// cannot pick a binary for a repository that does not exist yet.
	refBackend := req.ReferenceBackend
	if refBackend.Name != "" {
		exists, err := repositoryExists(ctx, repo)
		if err != nil {
			return fmt.Errorf("manager: %w", err)
		}

		if backup.Empty || !exists {
			ctx = gitcmd.ContextWithReferenceBackend(ctx, refBackend)
		} else {
			refBackend = git.ReferenceBackend{}
		}
	}

	switch {
	case backup.NonExistent:
		return nil // Nothing to restore for non-existent repositories
	case backup.Empty:
		if err := recreateRepo(ctx, repo, backup, refBackend); err != nil {
			return fmt.Errorf("manager: recreate empty repo: %w", err)
		}
	}

	// Git bundles can not be created for empty repositories.
	if !backup.Empty {
		// Restore Git objects, potentially from increments.
		if err := mgr.restoreFromRefs(ctx, repo, backup); err != nil {
			mgr.logger.WithFields(log.Fields{
				"storage":       req.Repository.GetStorageName(),
				"relative_path": req.Repository.GetRelativePath(),
				"backup_id":     backup.ID,
				logrus.ErrorKey: err,
			}).Warn("unable to reset refs. Proceeding with a normal restore")

			// If we can't reset the refs, perform a full restore by recreating the repo and cloning from the bundle.
			if err := mgr.restoreFromBundle(ctx, repo, backup, refBackend); err != nil {
				return fmt.Errorf("manager: restore from bundle: %w", err)
			}
		}
	}

	// Restore custom hooks. Each custom hooks archive contains the entirety of the hooks, so
	// we can just restore the most recent archive.
	latestStep := backup.Steps[len(backup.Steps)-1]
	return mgr.restoreCustomHooks(ctx, repo, latestStep.CustomHooksPath)
}

// WriteBackupID writes the given backup id to the backup sink.
func (mgr *IDManager) WriteBackupID(ctx context.Context, req *WriteBackupIDRequest) error {
	key := filepath.Join(req.PathPrefix, backupIDPrefix, req.BackupID)
	w, err := mgr.sink.GetWriter(ctx, key)
	if err != nil {
		return fmt.Errorf("write backup marker: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("write backup marker: close: %w", err)
	}

	return nil
}

// ReadLatestBackupID returns the backup ID of the most recent completed backup run.
// This iterates through backup_ids/ path on the object storage and finds the latest
// entry according to the object's modification time.
func (mgr *IDManager) ReadLatestBackupID(ctx context.Context, pathPrefix string) (string, error) {
	searchPath := filepath.Join(pathPrefix, backupIDPrefix)
	iter := mgr.sink.List(searchPath)

	var latestKey string
	var latestModTime time.Time
	for iter.Next(ctx) {
		modTime := iter.ModTime()
		// Use modification time as primary sort key, with lexicographic
		// tiebreaker when timestamps are equal (e.g. rapid writes on
		// filesystems with coarse time resolution).
		if latestKey == "" || modTime.After(latestModTime) || (modTime.Equal(latestModTime) && iter.Path() > latestKey) {
			latestKey = iter.Path()
			latestModTime = modTime
		}
	}

	if err := iter.Err(); err != nil {
		return "", fmt.Errorf("find latest backup ID: %w", err)
	}

	if latestKey == "" {
		return "", ErrDoesntExist
	}

	return path.Base(latestKey), nil
}

func (mgr *Manager) restoreFromRefs(ctx context.Context, repo Repository, backup *Backup) error {
	latestStep := backup.Steps[len(backup.Steps)-1]
	refs, err := mgr.readRefs(ctx, latestStep.RefPath)
	if err != nil {
		return fmt.Errorf("read refs from backup: %w", err)
	}
	if len(refs) == 0 {
		return errors.New("no refs in backup")
	}

	// Reset all refs except for HEAD.
	if err := repo.ResetRefs(ctx, refs, false); err != nil {
		return fmt.Errorf("reset refs: %w", err)
	}

	// Explicitly reset HEAD to the default branch tracked by the manifest if available. In a
	// bundle restore, this would've been done during repository creation.
	headRef := git.ReferenceName(backup.HeadReference)
	if headRef == "" {
		return errors.New("expected HEAD to be a symbolic reference")
	}

	return repo.SetHeadReference(ctx, headRef)
}

func (mgr *Manager) restoreFromBundle(ctx context.Context, repo Repository, backup *Backup, refBackend git.ReferenceBackend) error {
	defaultBranchKnown, err := createEmptyRepoIfNeeded(ctx, repo, backup, refBackend)
	if err != nil {
		return fmt.Errorf("create empty repo: %w", err)
	}

	for _, step := range backup.Steps {
		refs, err := mgr.readRefs(ctx, step.RefPath)
		if err != nil {
			return fmt.Errorf("read refs: %w", err)
		}

		// In case old manifest gets loaded and since empty repository
		// backups do not contain a bundle, skip bundle restoration.
		// TODO: Remove refs check after couple of releases.
		if len(refs) > 0 {
			if err := mgr.restoreBundle(ctx, repo, step.BundlePath, !defaultBranchKnown); err != nil {
				return fmt.Errorf("restore bundle: %w", err)
			}
		}
	}

	// After all objects are fetched, update refs optimistically based on the latest refs
	// We need this because fetch bundle would not remove refs that are not in the bundle.
	// This ignores failures on ref updates and proceeds optimistically.
	if err := mgr.restoreRefsOptimistically(ctx, repo, backup); err != nil {
		return fmt.Errorf("restore refs optimistically: %w", err)
	}

	// Explicitly reset HEAD to the default branch tracked by the manifest if available.
	if defaultBranchKnown {
		if err := repo.SetHeadReference(ctx, git.ReferenceName(backup.HeadReference)); err != nil {
			return fmt.Errorf("set head reference: %w", err)
		}
	}

	return nil
}

// restoreRefsOptimistically updates refs from the backup, ignoring failures for missing objects.
func (mgr *Manager) restoreRefsOptimistically(ctx context.Context, repo Repository, backup *Backup) error {
	latestStep := backup.Steps[len(backup.Steps)-1]
	refs, err := mgr.readRefs(ctx, latestStep.RefPath)
	if err != nil {
		return fmt.Errorf("read refs from backup: %w", err)
	}

	if len(refs) == 0 {
		return nil
	}

	if err := repo.ResetRefs(ctx, refs, true); err != nil {
		return fmt.Errorf("update refs optimistically: %w", err)
	}

	return nil
}

func removeRepository(ctx context.Context, repo Repository, cause error) error {
	if err := repo.Remove(ctx); err != nil {
		return fmt.Errorf("manager: repository removed: %w", err)
	}

	return cause
}

// setContextServerInfo overwrites server with gitaly connection info from ctx metadata when server is zero.
func setContextServerInfo(ctx context.Context, server *storage.ServerInfo, storageName string) error {
	if !server.Zero() {
		return nil
	}

	var err error
	*server, err = storage.ExtractGitalyServer(ctx, storageName)
	if err != nil {
		return fmt.Errorf("set context server info: %w", err)
	}

	return nil
}

func (mgr *Manager) writeBundle(ctx context.Context, repo Repository, step *Step) (returnErr error) {
	timer := prometheus.NewTimer(backupLatency.WithLabelValues("bundle"))
	defer timer.ObserveDuration()

	var patterns io.Reader
	if len(step.PreviousRefPath) > 0 {
		// If there is a previous ref path, then we are creating an increment
		negatedRefs, err := mgr.negatedKnownRefs(ctx, step)
		if err != nil {
			return fmt.Errorf("write bundle: %w", err)
		}
		defer func() {
			if err := negatedRefs.Close(); err != nil && returnErr == nil {
				returnErr = fmt.Errorf("write bundle: %w", err)
			}
		}()

		patterns = io.MultiReader(strings.NewReader("--all\n"), negatedRefs)
	}

	w := NewLazyWriter(func() (io.WriteCloser, error) {
		return mgr.sink.GetWriter(ctx, step.BundlePath)
	})
	defer func() {
		backupBundleSize.Observe(float64(w.BytesWritten()))
		if err := w.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("write bundle: %w", err))
		}
	}()

	if err := repo.CreateBundle(ctx, w, patterns); err != nil {
		if errors.Is(err, localrepo.ErrEmptyBundle) {
			return nil
		}
		return fmt.Errorf("write bundle: %w", err)
	}

	return nil
}

func (mgr *Manager) negatedKnownRefs(ctx context.Context, step *Step) (io.ReadCloser, error) {
	if len(step.PreviousRefPath) == 0 {
		return io.NopCloser(new(bytes.Reader)), nil
	}

	r, w := io.Pipe()

	go func() {
		defer w.Close()

		reader, err := mgr.sink.GetReader(ctx, step.PreviousRefPath)
		if err != nil {
			_ = w.CloseWithError(err)
			return
		}
		defer reader.Close()

		d := gitcmd.NewShowRefDecoder(reader)
		for {
			var ref git.Reference

			if err := d.Decode(&ref); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				_ = w.CloseWithError(err)
				return
			}

			if _, err := fmt.Fprintf(w, "^%s\n", ref.Target); err != nil {
				_ = w.CloseWithError(err)
				return
			}
		}
	}()

	return r, nil
}

func (mgr *Manager) readRefs(ctx context.Context, path string) ([]git.Reference, error) {
	reader, err := mgr.sink.GetReader(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read refs: %w", err)
	}
	defer reader.Close()

	var refs []git.Reference

	d := gitcmd.NewShowRefDecoder(reader)
	for {
		var ref git.Reference

		if err := d.Decode(&ref); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return refs, fmt.Errorf("read refs: %w", err)
		}

		// HEAD is tracked as a symbolic reference in the backup manifest and will be restored separately.
		if ref.Name == "HEAD" {
			continue
		}

		refs = append(refs, ref)
	}

	return refs, nil
}

func (mgr *Manager) restoreBundle(ctx context.Context, repo Repository, path string, updateHead bool) error {
	reader, err := mgr.sink.GetReader(ctx, path)
	if err != nil {
		return fmt.Errorf("get reader: %q: %w", path, err)
	}
	defer reader.Close()

	if err := repo.FetchBundle(ctx, reader, updateHead); err != nil {
		return fmt.Errorf("fetch bundle: %q: %w", path, err)
	}
	return nil
}

func (mgr *Manager) writeCustomHooks(ctx context.Context, repo Repository, path string) (returnErr error) {
	timer := prometheus.NewTimer(backupLatency.WithLabelValues("custom_hooks"))
	defer timer.ObserveDuration()

	w := NewLazyWriter(func() (io.WriteCloser, error) {
		return mgr.sink.GetWriter(ctx, path)
	})
	defer func() {
		if err := w.Close(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("write custom hooks: %w", err)
		}
	}()
	if err := repo.GetCustomHooks(ctx, w); err != nil {
		return fmt.Errorf("write custom hooks: %w", err)
	}
	return nil
}

func (mgr *Manager) restoreCustomHooks(ctx context.Context, repo Repository, path string) error {
	reader, err := mgr.sink.GetReader(ctx, path)
	if err != nil {
		if errors.Is(err, ErrDoesntExist) {
			return nil
		}
		return fmt.Errorf("restore custom hooks: %w", err)
	}
	defer reader.Close()

	if err := repo.SetCustomHooks(ctx, reader); err != nil {
		return fmt.Errorf("restore custom hooks, %q: %w", path, err)
	}
	return nil
}

// createEmptyRepoIfNeeded creates empty repository if it does not exists yet
func createEmptyRepoIfNeeded(ctx context.Context, repo Repository, backup *Backup, refBackend git.ReferenceBackend) (bool, error) {
	defaultBranch, defaultBranchKnown := git.ReferenceName(backup.HeadReference).Branch()

	exists, err := repositoryExists(ctx, repo)
	if err != nil {
		return defaultBranchKnown, err
	}

	if !exists {
		hash, err := git.ObjectHashByFormat(backup.ObjectFormat)
		if err != nil {
			return defaultBranchKnown, fmt.Errorf("get object hash: %w", err)
		}

		if err := repo.Create(ctx, hash, refBackend, defaultBranch); err != nil {
			return defaultBranchKnown, fmt.Errorf("create repository: %w", err)
		}
	}

	return defaultBranchKnown, nil
}

// repositoryExists reports whether the repository exists.
func repositoryExists(ctx context.Context, repo Repository) (bool, error) {
	if _, err := repo.ObjectHash(ctx); err != nil {
		if status.Code(err) == codes.NotFound {
			return false, nil
		}

		return false, fmt.Errorf("check repository existence: %w", err)
	}

	return true, nil
}

// recreateRepo removes the existing repository and creates a new empty one.
func recreateRepo(ctx context.Context, repo Repository, backup *Backup, refBackend git.ReferenceBackend) error {
	if err := repo.Remove(ctx); err != nil {
		return err
	}

	if _, err := createEmptyRepoIfNeeded(ctx, repo, backup, refBackend); err != nil {
		return fmt.Errorf("create empty repo: %w", err)
	}

	return nil
}
