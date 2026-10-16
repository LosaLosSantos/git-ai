// Package gate holds the gRPC interceptors of the authoritative storage mode:
// which RPCs may run, the catch-up before serving, and the registration and
// removal of repositories. Owned by WP56 (doc/graft/PLAN6.md, "Gate",
// binding).
//
// In authoritative mode every repository-scoped RPC goes through the gate:
//
//   - HookService and StorageCoreService RPCs, unknown methods and RPCs of
//     other scopes pass through untouched (hooks run while git holds reference
//     locks, so they never catch up).
//   - A MUTATOR outside AuthoritativeMutators is rejected with
//     FailedPrecondition before its handler runs.
//   - CreateRepository runs its handler (with the reftable backend), then
//     registers the new repository with a first changeset holding its HEAD.
//   - RemoveRepository deletes the database row first, then runs its handler.
//   - Every other RPC catches the local repository up (Freshener.EnsureFresh)
//     before its handler runs. For a streaming RPC this happens on the first
//     RecvMsg, which carries the request.
//   - A target repository whose relative path is not canonical (for example
//     "grp//proj.git" or "./proj.git") is rejected with InvalidArgument: the
//     database key is the relative path as given, so two spellings of one
//     directory would otherwise be two repositories in the database.
//
// Stale caches: a repository removed on another node leaves the local copies
// of the other nodes behind. A local repository that has a watermark (so the
// storage core wrote it: it was created, imported or hydrated here) while the
// database no longer has its key is such a stale cache. The catch-up moves it
// into the storage's temporary directory itself and reports NotFound; the gate
// moves it the same way before CreateRepository runs, and, as a fallback, when
// EnsureFresh reports it as "not imported", then lets the handler answer as for
// any missing repository. A local repository without a
// watermark was never in the storage core: it stays untracked
// (FailedPrecondition). CreateRepository, RemoveRepository and the moves of
// stale caches of one repository are serialized on the node.
//
// Shadow history (doc/graft/PLAN6.md, LOCK AND HISTORY RULE): shadow mode
// writes the file changelog, not the database one. A tracked repository whose
// local file changelog has entries beyond the database's log sequence still
// has shadow-period history that is not in the database, and serving it would
// make the database changelog reuse those sequences. Shadow mode also writes
// the file review store only, so a file review that ImportHistory would still
// write (corestore.ReviewNeedsImport: created or changed in shadow mode) is
// shadow-period history too. The gate refuses such a repository with
// FailedPrecondition until ImportRepository brought the history in. The check
// runs before the catch-up, on the local repository as it is (the catch-up may
// replace it by a hydrated copy without that history), reads the last
// sequence of the file log (cheap) and the file reviews, and is remembered per
// repository once it passed.
package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	reviewstore "github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	createRepositoryMethod = "/gitaly.RepositoryService/CreateRepository"
	removeRepositoryMethod = "/gitaly.RepositoryService/RemoveRepository"

	// operationCreateRepository is the operation of the first changeset of a
	// repository created through CreateRepository.
	operationCreateRepository = "create_repository"

	// Patterns of the directories, in the storage's temporary directory, that
	// local repositories are moved into.
	createdPattern = "graft-created-*"
	stalePattern   = "graft-stale-*"
)

// Gate is the authoritative-mode gate. When the mode is not authoritative its
// interceptors call the handler directly.
type Gate struct {
	core        *corestore.Core
	fresh       corestore.Freshener
	logger      log.Logger
	locator     storage.Locator
	repoFactory localrepo.Factory
	registry    *protoregistry.Registry

	// mu protects locks.
	mu sync.Mutex
	// locks serializes CreateRepository, RemoveRepository and the moves of
	// stale caches per local repository path. Entries are removed when no
	// goroutine refers to them.
	locks map[string]*pathLock

	// historyChecked holds the keys (corestore.RepoKey.String()) of the
	// repositories whose file changelog is known to be in the database, see
	// checkHistory.
	historyChecked sync.Map
}

type pathLock struct {
	mu   sync.Mutex
	refs int
}

// New returns the gate.
func New(core *corestore.Core, fresh corestore.Freshener, logger log.Logger, locator storage.Locator, repoFactory localrepo.Factory, registry *protoregistry.Registry) *Gate {
	return &Gate{
		core:        core,
		fresh:       fresh,
		logger:      logger,
		locator:     locator,
		repoFactory: repoFactory,
		registry:    registry,
		locks:       map[string]*pathLock{},
	}
}

// UnaryInterceptor returns the unary interceptor.
func (g *Gate) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !g.core.Authoritative() {
			return handler(ctx, req)
		}

		mi, scoped := g.scoped(info.FullMethod)
		if !scoped {
			return handler(ctx, req)
		}
		if err := checkAllowed(info.FullMethod, mi); err != nil {
			return nil, err
		}

		repo := targetRepo(mi, req)
		if repo == nil {
			// Without a target repository the handler rejects the request itself.
			return handler(ctx, req)
		}
		if err := checkCanonical(repo); err != nil {
			return nil, err
		}

		switch info.FullMethod {
		case createRepositoryMethod:
			return g.createRepository(ctx, req, repo, handler)
		case removeRepositoryMethod:
			return g.removeRepository(ctx, req, repo, handler)
		default:
			if err := g.ensureFresh(ctx, repo); err != nil {
				return nil, err
			}
			return handler(ctx, req)
		}
	}
}

// StreamInterceptor returns the stream interceptor. A mutator outside the
// allowlist is rejected at once; otherwise the catch-up runs on the first
// RecvMsg, which reads the request, before the handler gets it.
func (g *Gate) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !g.core.Authoritative() {
			return handler(srv, stream)
		}

		mi, scoped := g.scoped(info.FullMethod)
		if !scoped {
			return handler(srv, stream)
		}
		if err := checkAllowed(info.FullMethod, mi); err != nil {
			return err
		}

		return handler(srv, &firstRecvStream{ServerStream: stream, gate: g, mi: mi})
	}
}

// firstRecvStream catches up the target repository of the first received
// message before handing that message to the handler.
type firstRecvStream struct {
	grpc.ServerStream

	gate    *Gate
	mi      protoregistry.MethodInfo
	checked bool
}

func (s *firstRecvStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	if s.checked {
		return nil
	}
	s.checked = true

	repo := targetRepo(s.mi, m)
	if repo == nil {
		return nil
	}
	if err := checkCanonical(repo); err != nil {
		return err
	}
	return s.gate.ensureFresh(s.ServerStream.Context(), repo)
}

// scoped returns the method info of a repository-scoped RPC that the gate
// checks. Hook and storage core RPCs, unknown methods and RPCs of other scopes
// are not checked.
func (g *Gate) scoped(fullMethod string) (protoregistry.MethodInfo, bool) {
	for _, prefix := range Passthrough {
		if strings.HasPrefix(fullMethod, prefix) {
			return protoregistry.MethodInfo{}, false
		}
	}

	mi, err := g.registry.LookupMethod(fullMethod)
	if err != nil || mi.Scope != protoregistry.ScopeRepository {
		return protoregistry.MethodInfo{}, false
	}
	return mi, true
}

// checkAllowed rejects a mutator outside the allowlist.
func checkAllowed(fullMethod string, mi protoregistry.MethodInfo) error {
	if mi.Operation != protoregistry.OpMutator {
		return nil
	}
	if _, ok := AuthoritativeMutators[fullMethod]; ok {
		return nil
	}
	return structerr.NewFailedPrecondition("%s is not supported in authoritative storage mode", fullMethod)
}

// targetRepo returns the target repository of the request, nil when it has
// none.
func targetRepo(mi protoregistry.MethodInfo, msg any) *gitalypb.Repository {
	pm, ok := msg.(proto.Message)
	if !ok {
		return nil
	}
	repo, err := mi.TargetRepo(pm)
	if err != nil {
		return nil
	}
	return repo
}

// checkCanonical rejects a relative path that is not in its cleaned form. The
// database key of a repository is its relative path as given
// (corestore.KeyOf), while the locator resolves every spelling of a path to
// the same directory: without this check "grp//proj.git" and "grp/proj.git"
// would be two repositories in the database sharing one local copy. An empty
// path is left to the handler, which rejects it itself.
func checkCanonical(repo *gitalypb.Repository) error {
	rel := repo.GetRelativePath()
	if rel == "" {
		return nil
	}
	if clean := filepath.Clean(rel); clean != rel {
		return structerr.NewInvalidArgument("relative path %q is not canonical in authoritative storage mode, use %q", rel, clean)
	}
	return nil
}

// ensureFresh catches up the local repository. A repository that exists
// neither in the database nor locally (NotFound) is left to the handler, which
// answers as it does for any missing repository (NotFound, or false for
// RepositoryExists). So is a stale cache of a removed repository, after it has
// been moved away (see dropStale).
//
// The shadow history guard (checkHistory) runs first, on the local repository
// as it is: the catch-up may replace it (a files-backend copy is replaced by a
// reftable one, REFTABLE RULE), and the replacement's empty file changelog and
// review store would hide the history the guard is there to protect.
func (g *Gate) ensureFresh(ctx context.Context, repo *gitalypb.Repository) error {
	if err := g.checkHistory(ctx, repo); err != nil {
		return err
	}

	err := g.fresh.EnsureFresh(ctx, repo)
	switch status.Code(err) {
	case codes.OK:
		return nil
	case codes.NotFound:
		return nil
	case codes.FailedPrecondition:
		// The database does not know the repository but a local one exists, or
		// the repository's import is not complete.
		state, staleErr := g.dropStale(ctx, repo)
		if staleErr != nil {
			return staleErr
		}
		switch state {
		case localGone:
			return nil
		case localTracked:
			// The repository was registered (on another node) since the catch-up
			// read the database: check its history (the first check found no
			// record to compare with) and catch up again, once.
			if err := g.checkHistory(ctx, repo); err != nil {
				return err
			}
			err = g.fresh.EnsureFresh(ctx, repo)
			switch status.Code(err) {
			case codes.OK, codes.NotFound:
				return nil
			}
			return err
		}
		return err
	default:
		return err
	}
}

// checkHistory refuses a tracked repository whose local file changelog has
// entries with a sequence greater than the database's log sequence: history
// written in shadow mode that ImportRepository has not brought into the
// database yet (doc/graft/PLAN6.md, LOCK AND HISTORY RULE), or whose file
// review store has reviews that are not in the database yet (checkReviews).
// Authoritative mode never writes the file changelog or the file review store,
// so a repository that passed once keeps passing: the result is remembered for
// the life of the process.
func (g *Gate) checkHistory(ctx context.Context, repo *gitalypb.Repository) error {
	key := corestore.KeyOf(repo).String()
	if _, ok := g.historyChecked.Load(key); ok {
		return nil
	}

	path, err := g.repoPath(ctx, repo)
	if err != nil {
		return err
	}

	last, err := changelog.LastSequence(path)
	if err != nil {
		return structerr.NewInternal("storage core: reading the last sequence of the file changelog: %w", err)
	}

	fileReviews, err := reviewstore.Open(path).List("", true, 0)
	if err != nil {
		return structerr.NewInternal("storage core: reading the file reviews: %w", err)
	}

	if last > 0 || len(fileReviews) > 0 {
		r, err := g.core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
		switch {
		case errors.Is(err, corestore.ErrRepositoryNotFound):
			// Untracked, or the stale cache of a removed repository: the catch-up
			// decides, and the check is not remembered.
			return nil
		case err != nil:
			return structerr.NewUnavailable("storage core: get repository: %w", err)
		case last > r.LogSeq:
			return errShadowHistory().
				WithMetadata("file_changelog_sequence", last).
				WithMetadata("database_changelog_sequence", r.LogSeq)
		}

		if err := g.checkReviews(ctx, r.ID, fileReviews); err != nil {
			return err
		}
	}

	g.historyChecked.Store(key, struct{}{})
	return nil
}

// checkReviews refuses a repository whose file review store has a review that
// ImportHistory would still write into the database (corestore.ReviewNeedsImport):
// one created or changed in shadow mode since the last import. Without this
// check a switch that skipped the import would serve stale reviews, and the
// database would hand out the IDs of the shadow-mode reviews again.
func (g *Gate) checkReviews(ctx context.Context, repoID int64, fileReviews []*gitalypb.Review) error {
	if len(fileReviews) == 0 {
		return nil
	}

	dbReviews, err := g.core.Meta.ListReviews(ctx, repoID, "", true, 0)
	if err != nil {
		return structerr.NewUnavailable("storage core: list reviews: %w", err)
	}

	byID := make(map[string]*gitalypb.Review, len(dbReviews))
	for _, r := range dbReviews {
		byID[r.GetId()] = r
	}

	for _, file := range fileReviews {
		if corestore.ReviewNeedsImport(file, byID[file.GetId()]) {
			return errShadowHistory().WithMetadata("review_id", file.GetId())
		}
	}

	return nil
}

// errShadowHistory is the error of a repository whose shadow-mode history is
// not in the database yet.
func errShadowHistory() structerr.Error {
	return structerr.NewFailedPrecondition("the shadow-mode history of this repository is not in the storage core yet: run graft storage import")
}

// localState is what dropStale found.
type localState int

const (
	// localUntracked: a local repository without a watermark that the database
	// does not know. It is left in place.
	localUntracked localState = iota
	// localGone: the database does not know the repository and there is no
	// local repository (any more): a stale cache was moved away, or there was
	// nothing to move.
	localGone
	// localTracked: the database knows the repository. Nothing was moved.
	localTracked
)

// dropStale moves a stale cache of a removed repository away: a local
// repository with a watermark (so the storage core wrote it) whose key the
// database does not have. It runs under the repository's gate lock.
func (g *Gate) dropStale(ctx context.Context, repo *gitalypb.Repository) (localState, error) {
	path, err := g.repoPath(ctx, repo)
	if err != nil {
		return localUntracked, err
	}
	unlock := g.lock(path)
	defer unlock()

	return g.dropStaleLocked(ctx, repo, path)
}

// dropStaleLocked is dropStale for a caller that holds the gate lock of path.
// The local repository is inspected before the database is read, so that a
// repository registered meanwhile (whose watermark is written after the
// registration) is never taken for a stale one.
func (g *Gate) dropStaleLocked(ctx context.Context, repo *gitalypb.Repository, path string) (localState, error) {
	exists := true
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		exists = false
	} else if err != nil {
		return localUntracked, structerr.NewInternal("stat repository: %w", err)
	}

	hasWatermark := false
	if exists {
		// A corrupt watermark is not proof that the storage core wrote the
		// repository: it is treated as missing, so the repository is kept.
		_, ok, err := gitops.ReadWatermark(path)
		hasWatermark = err == nil && ok
	}

	r, err := g.core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
	switch {
	case err == nil:
		if r.HeadSeq == 0 {
			// An import that is not complete: the syncer reports it.
			return localUntracked, nil
		}
		return localTracked, nil
	case !errors.Is(err, corestore.ErrRepositoryNotFound):
		return localUntracked, structerr.NewUnavailable("storage core: get repository: %w", err)
	case !exists:
		return localGone, nil
	case !hasWatermark:
		return localUntracked, nil
	}

	if err := g.moveAside(repo, path, stalePattern); err != nil {
		return localUntracked, structerr.NewInternal("storage core: moving away a stale cache of a removed repository: %w", err)
	}
	g.logger.WithField("repository_path", path).InfoContext(ctx, "storage core: moved away the stale cache of a removed repository")
	return localGone, nil
}

// createRepository runs CreateRepository with the reftable backend (REFTABLE
// RULE), then registers the new repository in the database. CreateRepository
// does not catch up first: the repository is new. A stale cache of a removed
// repository at the same path is moved away first, so that the handler creates
// a new repository instead of reporting the stale one as existing.
func (g *Gate) createRepository(ctx context.Context, req any, repo *gitalypb.Repository, handler grpc.UnaryHandler) (any, error) {
	createReq, ok := req.(*gitalypb.CreateRepositoryRequest)
	if !ok {
		return nil, structerr.NewInternal("unexpected request type %T", req)
	}

	switch createReq.GetReferenceBackend() {
	case gitalypb.ReferenceBackend_REFERENCE_BACKEND_UNSPECIFIED:
		// Left unspecified, the handler lets the node choose the backend (MVCC
		// when its feature flag is on): ask for reftable explicitly.
		createReq = proto.Clone(createReq).(*gitalypb.CreateRepositoryRequest)
		createReq.ReferenceBackend = gitalypb.ReferenceBackend_REFERENCE_BACKEND_REFTABLE
	case gitalypb.ReferenceBackend_REFERENCE_BACKEND_REFTABLE:
	default:
		return nil, structerr.NewFailedPrecondition("authoritative storage mode requires a reftable repository")
	}
	ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendReftables)

	path, err := g.repoPath(ctx, repo)
	if err != nil {
		return nil, err
	}
	unlock := g.lock(path)
	defer unlock()

	if _, err := g.dropStaleLocked(ctx, repo, path); err != nil {
		return nil, err
	}

	resp, err := handler(ctx, createReq)
	if err != nil {
		return resp, err
	}
	if err := g.registerCreated(ctx, repo, path); err != nil {
		return nil, err
	}
	return resp, nil
}

// registerCreated registers the repository that CreateRepository just created
// locally at path, with a first changeset holding its HEAD, and writes the
// local watermark. When the registration fails (in particular when the
// database already knows the repository) the new local repository is moved
// into the storage's temporary directory, so that it does not stay behind
// untracked.
func (g *Gate) registerCreated(ctx context.Context, repo *gitalypb.Repository, path string) error {
	registered, err := g.register(ctx, repo)
	if err != nil {
		if moveErr := g.moveAside(repo, path, createdPattern); moveErr != nil {
			g.logger.WithError(moveErr).ErrorContext(ctx, "storage core: moving aside a created repository that could not be registered")
		}
		return err
	}

	if err := gitops.WriteWatermark(path, corestore.Watermark{RepoID: registered.ID, Seq: registered.HeadSeq}); err != nil {
		// The repository is registered: without a watermark the next catch-up
		// replaces the local copy by a hydrated one, so the RPC still succeeded.
		g.logger.WithError(err).WarnContext(ctx, "storage core: writing the watermark of a created repository")
	}
	return nil
}

// register records the local repository in the database.
func (g *Gate) register(ctx context.Context, repo *gitalypb.Repository) (corestore.Repository, error) {
	local := g.repoFactory.Build(repo)

	hash, err := local.ObjectHash(ctx)
	if err != nil {
		return corestore.Repository{}, structerr.NewInternal("object format: %w", err)
	}
	refs, err := gitops.ReadRefs(ctx, local)
	if err != nil {
		return corestore.Repository{}, structerr.NewInternal("read references: %w", err)
	}
	head, ok := refs["HEAD"]
	if !ok {
		return corestore.Repository{}, structerr.NewInternal("created repository has no HEAD")
	}

	registered, _, err := g.core.Meta.RegisterRepository(ctx, corestore.KeyOf(repo), hash.Format, corestore.InitialState{
		Changeset: &corestore.ChangesetInput{
			Updates:   []corestore.RefUpdate{{Name: "HEAD", Expect: corestore.ExpectValue, New: head}},
			Operation: operationCreateRepository,
			Actor:     identity.Actor(ctx, "unknown"),
			NodeID:    g.core.NodeID,
		},
	})
	switch {
	case errors.Is(err, corestore.ErrRepositoryExists):
		return corestore.Repository{}, structerr.NewAlreadyExists("repository exists already")
	case err != nil:
		return corestore.Repository{}, structerr.NewUnavailable("storage core: register repository: %w", err)
	}
	return registered, nil
}

// removeRepository deletes the database row first, then runs the handler. A
// repository that was tracked and has no local copy on this node is removed
// successfully; an untracked one is just removed locally.
//
// The gate cannot stop a hydration of the repository that is already running
// on this node: it may rename its copy into place after the handler removed
// the directory. Such a copy has a watermark and its key is no longer in the
// database, so it is a stale cache: it is moved away here when it is already
// back, and otherwise by the next request that reaches it (see dropStale).
func (g *Gate) removeRepository(ctx context.Context, req any, repo *gitalypb.Repository, handler grpc.UnaryHandler) (any, error) {
	path, err := g.repoPath(ctx, repo)
	if err != nil {
		return nil, err
	}
	unlock := g.lock(path)
	defer unlock()

	deleted, err := g.core.Meta.DeleteRepository(ctx, corestore.KeyOf(repo))
	if err != nil {
		return nil, structerr.NewUnavailable("storage core: delete repository: %w", err)
	}

	resp, err := handler(ctx, req)
	if err != nil && deleted && status.Code(err) == codes.NotFound {
		resp, err = &gitalypb.RemoveRepositoryResponse{}, nil
	}
	if err == nil && deleted {
		if _, staleErr := g.dropStaleLocked(ctx, repo, path); staleErr != nil {
			g.logger.WithError(staleErr).WarnContext(ctx, "storage core: checking for a stale cache after removing a repository")
		}
	}
	return resp, err
}

// repoPath returns the path of the local repository, whether it exists or not.
// The locator's errors (an unknown storage, an invalid relative path) are
// returned as they are, like the handlers return them.
func (g *Gate) repoPath(ctx context.Context, repo *gitalypb.Repository) (string, error) {
	return g.locator.GetRepoPath(ctx, repo, storage.WithRepositoryVerificationSkipped())
}

// moveAside moves the local repository at path into a new directory, named
// after pattern, of the storage's temporary directory. Nothing is deleted in
// place; the storage's temporary directory is cleaned up by Gitaly.
func (g *Gate) moveAside(repo *gitalypb.Repository, path, pattern string) error {
	tmp, err := g.locator.TempDir(repo.GetStorageName())
	if err != nil {
		return fmt.Errorf("temporary directory: %w", err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(tmp, pattern)
	if err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(dir, "repo.git")); err != nil {
		_ = os.Remove(dir)
		return err
	}
	return nil
}

// lock takes the gate lock of the local repository at path and returns the
// function that releases it.
func (g *Gate) lock(path string) func() {
	g.mu.Lock()
	l := g.locks[path]
	if l == nil {
		l = &pathLock{}
		g.locks[path] = l
	}
	l.refs++
	g.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()

		g.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(g.locks, path)
		}
		g.mu.Unlock()
	}
}
