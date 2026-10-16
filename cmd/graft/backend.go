package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/client"
	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// sdkBackend implements workspace.Backend on top of the public sdk. It is bound
// to one repository of one server.
type sdkBackend struct {
	client  *sdk.Client
	repo    *sdk.Repo
	address string
	token   string
	// userID is the configured acting user ID (gl_id) of writes; see user.
	userID string

	// mu guards the lazily dialed OperationService connection used to delete
	// temporary branches. The sdk does not expose its own connection.
	mu      sync.Mutex
	opsConn *grpc.ClientConn
	ops     gitalypb.OperationServiceClient
}

// dialBackend connects to the server in t and returns a backend bound to the
// repository in t. The connection is closed when the command in ctx finishes.
func dialBackend(ctx context.Context, t target) (workspace.Backend, error) {
	b, err := openSDKBackend(ctx, t)
	if err != nil {
		return nil, err
	}
	registerCloser(ctx, b)
	return b, nil
}

// closerKey is the context key of the closers of one command.
type closerKey struct{}

// closers collects the resources that a command opens and must release.
type closers struct {
	mu    sync.Mutex
	items []io.Closer
}

// withClosers returns a context that carries a closer list, and a function that
// closes everything registered in it.
func withClosers(ctx context.Context) (context.Context, func()) {
	list := &closers{}
	return context.WithValue(ctx, closerKey{}, list), func() {
		list.mu.Lock()
		defer list.mu.Unlock()
		for _, c := range list.items {
			_ = c.Close()
		}
		list.items = nil
	}
}

// registerCloser closes c when the command in ctx finishes. Without a closer
// list in ctx, c stays open.
func registerCloser(ctx context.Context, c io.Closer) {
	list, ok := ctx.Value(closerKey{}).(*closers)
	if !ok {
		return
	}
	list.mu.Lock()
	defer list.mu.Unlock()
	list.items = append(list.items, c)
}

// openSDKBackend is dialBackend with the concrete type.
func openSDKBackend(ctx context.Context, t target) (*sdkBackend, error) {
	if t.Address == "" {
		return nil, usagef("no server address: pass -address or set GRAFT_ADDRESS")
	}
	if t.Repo == "" {
		return nil, errors.New("workspace does not name a repository")
	}

	opts := []sdk.DialOption{sdk.WithStorage(pick(t.Storage, defaultStorage))}
	if t.Token != "" {
		// The HMAC v2 format is accepted for the shared token and for agent
		// tokens, which are used as the secret.
		opts = append(opts, sdk.WithToken(t.Token))
	}

	c, err := sdk.Dial(ctx, t.Address, opts...)
	if err != nil {
		return nil, err
	}
	return &sdkBackend{
		client:  c,
		repo:    c.Repo(t.Repo),
		address: t.Address,
		token:   t.Token,
		userID:  t.User,
	}, nil
}

// anonymousUser is the user ID of writes when none is configured. It is the
// name a standalone server gives to callers without an identity.
const anonymousUser = "anonymous"

// user returns the acting user of a write: the configured user ID (gl_id),
// which the server's access policy checks, and name and email, which the
// server records.
//
// A standalone server fills an empty user ID only in top-level request fields
// (with the identity of the access token), so with no configured user ID a
// land is checked as the token's identity. UserCommitFiles carries its user in
// a nested header that the server does not fill, and its hooks refuse a write
// without a user ID, so commits fall back to anonymousUser. Whatever the user
// ID, the changeset log records the identity of the access token when there is
// one.
func (b *sdkBackend) user(name, email string, needsID bool) sdk.User {
	id := b.userID
	if id == "" && needsID {
		id = anonymousUser
	}
	return sdk.User{ID: id, Username: id, Name: name, Email: email}
}

// Close releases the connections of the backend.
func (b *sdkBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	var err error
	if b.opsConn != nil {
		err = b.opsConn.Close()
	}
	return errors.Join(err, b.client.Close())
}

// ResolveRevision implements workspace.Backend.
func (b *sdkBackend) ResolveRevision(ctx context.Context, rev string) (string, error) {
	oid, err := b.repo.ResolveRevision(ctx, rev)
	if err != nil {
		return "", mapError(err)
	}
	return oid, nil
}

// ListTree implements workspace.Backend.
func (b *sdkBackend) ListTree(ctx context.Context, rev, path string, recursive bool) ([]workspace.Entry, error) {
	entries, err := b.repo.ListTree(ctx, rev, path, recursive)
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]workspace.Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, workspace.Entry{
			Path: e.Path,
			OID:  e.OID,
			Type: objectType(e.Type),
			Mode: uint32(e.Mode),
			Size: e.Size,
		})
	}
	return out, nil
}

// GetObjects implements workspace.Backend. Missing objects are left out.
func (b *sdkBackend) GetObjects(ctx context.Context, oids []string) (map[string][]byte, error) {
	objects, err := b.repo.GetObjects(ctx, oids)
	if err != nil {
		return nil, err
	}

	out := make(map[string][]byte, len(objects))
	for _, o := range objects {
		if !o.Missing {
			out[o.OID] = o.Data
		}
	}
	return out, nil
}

// ReadFile implements workspace.Backend.
func (b *sdkBackend) ReadFile(ctx context.Context, rev, path string) ([]byte, error) {
	content, err := b.repo.ReadFile(ctx, rev, path)
	if err != nil {
		return nil, mapError(err)
	}
	return content, nil
}

// CommitFiles implements workspace.Backend.
func (b *sdkBackend) CommitFiles(ctx context.Context, opts workspace.CommitOptions) (string, error) {
	actions, err := toSDKActions(opts.Actions)
	if err != nil {
		return "", err
	}

	res, err := b.repo.CommitFiles(ctx, sdk.CommitOptions{
		Branch:   opts.Branch,
		StartRev: opts.StartRev,
		Message:  opts.Message,
		Author:   sdk.Signature{Name: opts.Author.Name, Email: opts.Author.Email},
		User:     b.user(opts.Author.Name, opts.Author.Email, true),
		Actions:  actions,
	})
	if err != nil {
		return "", withUserHint(mapError(err), b.userID)
	}
	return res.CommitID, nil
}

// Land implements workspace.Backend.
func (b *sdkBackend) Land(ctx context.Context, opts workspace.LandOptions) (workspace.LandResult, error) {
	strategy, err := landStrategy(opts.Strategy)
	if err != nil {
		return workspace.LandResult{}, err
	}

	res, err := b.repo.Land(ctx, sdk.LandOptions{
		Branch:           opts.Branch,
		Commit:           opts.Commit,
		User:             b.user(opts.Committer.Name, opts.Committer.Email, false),
		Strategy:         strategy,
		CreateBranch:     opts.CreateBranch,
		ResolveConflicts: opts.ResolveConflicts,
	})
	switch {
	case sdk.IsMergeConflict(err):
		return workspace.LandResult{}, fmt.Errorf("%w: %s", workspace.ErrMergeConflict, status.Convert(err).Message())
	case sdk.IsNotAncestor(err):
		return workspace.LandResult{}, fmt.Errorf("%w: %s", workspace.ErrNotFastForward, status.Convert(err).Message())
	case err != nil:
		return workspace.LandResult{}, mapError(err)
	}

	return workspace.LandResult{
		Commit:        res.LandedCommitID,
		AlreadyLanded: res.AlreadyLanded,
		Rewritten:     res.Rewritten,
		ResolvedPaths: res.ResolvedPaths,
		Resolver:      res.Resolver,
		BatchSize:     res.Batch.Size,
		BatchPosition: res.Batch.Position,
		Attempts:      res.Attempts,
	}, nil
}

// DeleteBranch implements workspace.Backend. The sdk has no branch deletion, so
// OperationService.UserDeleteBranch is called over a connection of the backend.
// It is the RPC that agents may call: the reference-transaction policy decides
// on the deletion, so an agent can delete only the branches the policy opens
// to it, such as its own temporary branches. RefService.DeleteRefs is never
// allowed for agents, and calling it would leave the temporary branches behind.
func (b *sdkBackend) DeleteBranch(ctx context.Context, branch string) error {
	// A missing branch is not an error, as it was not with DeleteRefs. The RPC
	// itself reports it as FailedPrecondition.
	if _, err := b.ResolveRevision(ctx, "refs/heads/"+branch); errors.Is(err, workspace.ErrNotFound) {
		return nil
	}

	ops, err := b.operationService(ctx)
	if err != nil {
		return err
	}

	_, err = ops.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{
		Repository: b.repository(),
		BranchName: []byte(branch),
		// The user is filled in by the server from the identity of the token
		// when it is empty, and always for agents.
		User: &gitalypb.User{GlId: b.userID, GlUsername: b.userID},
	})
	return err
}

// Changesets implements workspace.Backend.
func (b *sdkBackend) Changesets(ctx context.Context, after uint64, limit int) ([]workspace.Changeset, error) {
	changesets, err := b.repo.Changesets(ctx, after, clampUint32(limit))
	if err != nil {
		return nil, err
	}

	out := make([]workspace.Changeset, 0, len(changesets))
	for _, c := range changesets {
		out = append(out, toChangeset(c))
	}
	return out, nil
}

// WatchChangesets implements workspace.Backend. The returned channels are closed
// when ctx is done or the watch ends. A watch failure is sent on the error
// channel before it closes.
func (b *sdkBackend) WatchChangesets(ctx context.Context, after uint64) (<-chan workspace.Changeset, <-chan error) {
	changes, watchErrs := b.repo.WatchChangesets(ctx, after)
	out := make(chan workspace.Changeset)
	errs := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errs)

		for c := range changes {
			select {
			case out <- toChangeset(c):
			case <-ctx.Done():
				return
			}
		}
		if err := <-watchErrs; err != nil {
			errs <- err
		}
	}()

	return out, errs
}

// Blame implements workspace.Backend.
func (b *sdkBackend) Blame(ctx context.Context, rev, path string) ([]workspace.BlameLine, error) {
	result, err := b.repo.Blame(ctx, rev, path)
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]workspace.BlameLine, 0, len(result.Lines))
	for _, l := range result.Lines {
		out = append(out, workspace.BlameLine{
			LineNumber:         int(l.LineNumber),
			CommitID:           l.CommitID,
			OriginalLineNumber: int(l.OriginalLineNumber),
			Content:            l.Content,
		})
	}
	return out, nil
}

// PathHistory implements workspace.Backend.
func (b *sdkBackend) PathHistory(ctx context.Context, rev, path string, limit int) ([]workspace.Commit, error) {
	commits, err := b.repo.PathHistory(ctx, rev, path, clampUint32(limit))
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]workspace.Commit, 0, len(commits))
	for _, c := range commits {
		out = append(out, workspace.Commit{
			ID:         c.GetId(),
			AuthorName: string(c.GetAuthor().GetName()),
			Subject:    string(c.GetSubject()),
		})
	}
	return out, nil
}

// operationService returns the OperationService client, dialing it on first use.
func (b *sdkBackend) operationService(ctx context.Context) (gitalypb.OperationServiceClient, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.ops != nil {
		return b.ops, nil
	}

	var grpcOpts []grpc.DialOption
	if b.token != "" {
		grpcOpts = append(grpcOpts, grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(b.token)))
	}
	conn, err := client.DialContext(ctx, b.address, client.WithGrpcOptions(grpcOpts))
	if err != nil {
		return nil, fmt.Errorf("dial operation service: %w", err)
	}

	b.opsConn = conn
	b.ops = gitalypb.NewOperationServiceClient(conn)
	return b.ops, nil
}

// repository returns the protobuf repository reference used by the RPCs of the backend. It
// carries the same GL repository identifier as the sdk requests.
func (b *sdkBackend) repository() *gitalypb.Repository {
	rel := b.repo.RelativePath()
	storage := b.client.Storage()
	return &gitalypb.Repository{
		StorageName:  storage,
		RelativePath: rel,
		GlRepository: "graft:" + storage + "/" + rel,
	}
}

// withUserHint explains a commit refused by the server's access policy when no
// user ID is configured, because the commit is then made as anonymousUser, on
// a temporary branch named after the author instead of the agent.
func withUserHint(err error, userID string) error {
	if err == nil || userID != "" || sdk.Code(err) != codes.PermissionDenied {
		return err
	}
	return fmt.Errorf("%w\nhint: no user ID is configured, so commits are made as user %q; set GRAFT_USER (or -user) to the agent name of your token", err, anonymousUser)
}

// mapError translates sdk errors that the workspace reacts to into the
// workspace sentinel errors. Other errors are returned unchanged.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	switch sdk.Code(err) {
	case codes.NotFound:
		return fmt.Errorf("%w: %s", workspace.ErrNotFound, status.Convert(err).Message())
	case codes.AlreadyExists:
		return fmt.Errorf("%w: %s", workspace.ErrAlreadyExists, status.Convert(err).Message())
	default:
		return err
	}
}

// toSDKActions converts workspace actions into sdk actions.
func toSDKActions(actions []workspace.Action) ([]sdk.Action, error) {
	out := make([]sdk.Action, 0, len(actions))
	for _, a := range actions {
		switch a.Kind {
		case workspace.ActionPut:
			out = append(out, sdk.Put{Path: a.Path, Content: a.Content, Executable: a.Executable, Overwrite: a.Overwrite})
		case workspace.ActionDelete:
			out = append(out, sdk.Delete{Path: a.Path})
		case workspace.ActionMove:
			out = append(out, sdk.Move{From: a.FromPath, To: a.Path})
		case workspace.ActionChmod:
			out = append(out, sdk.Chmod{Path: a.Path, Executable: a.Executable})
		default:
			return nil, fmt.Errorf("unknown action %q", a.Kind)
		}
	}
	return out, nil
}

// landStrategy maps a strategy name to the sdk strategy. An empty name selects
// rebase.
func landStrategy(name string) (sdk.LandStrategy, error) {
	switch name {
	case "", workspace.StrategyRebase:
		return sdk.LandRebase, nil
	case workspace.StrategyMerge:
		return sdk.LandMerge, nil
	case workspace.StrategyFFOnly:
		return sdk.LandFastForwardOnly, nil
	default:
		return 0, usagef("unknown strategy %q (want rebase, merge or ff_only)", name)
	}
}

// objectType maps an sdk object type to the workspace object type.
func objectType(t sdk.ObjectType) workspace.ObjectType {
	switch t {
	case sdk.ObjectBlob:
		return workspace.ObjectBlob
	case sdk.ObjectTree:
		return workspace.ObjectTree
	case sdk.ObjectCommit:
		return workspace.ObjectCommit
	default:
		return workspace.ObjectType(strings.ToLower(t.String()))
	}
}

func toChangeset(c *sdk.Changeset) workspace.Changeset {
	var members []workspace.ChangesetMember
	for _, m := range c.GetMembers() {
		member := workspace.ChangesetMember{
			OriginalCommit: m.GetOriginalCommitId(),
			Commit:         m.GetCommitId(),
			Actor:          m.GetActor(),
			Rewritten:      m.GetRewritten(),
		}
		for _, p := range m.GetResolvedPaths() {
			member.ResolvedPaths = append(member.ResolvedPaths, string(p))
		}
		members = append(members, member)
	}

	return workspace.Changeset{
		Sequence:   c.GetSequence(),
		Reference:  string(c.GetReference()),
		OldOID:     c.GetOldOid(),
		NewOID:     c.GetNewOid(),
		Operation:  c.GetOperation(),
		Actor:      c.GetActor(),
		RecordedAt: c.GetRecordedAt().AsTime(),
		Rewritten:  c.GetRewritten(),
		Members:    members,
	}
}

// clampUint32 converts a non-negative count to uint32. Negative values become
// zero, which selects the server default.
func clampUint32(n int) uint32 {
	if n <= 0 {
		return 0
	}
	if n > int(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(n)
}
