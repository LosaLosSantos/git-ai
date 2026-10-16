package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/client"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// refUpdater implements greenRefs on the server. Reading goes through the
// sdk. Moving uses RefService.UpdateReferences, which checks the old object ID
// in the server, so that a move never overwrites a concurrent change.
//
// The verifier moves green refs with the server's shared token, which is an
// operator credential. Agent tokens cannot write references outside the policy
// of the server, so the verifier is not an agent.
type refUpdater struct {
	repo       *sdk.Repo
	repository *gitalypb.Repository
	conn       *grpc.ClientConn
	refs       gitalypb.RefServiceClient
}

var _ greenRefs = (*refUpdater)(nil)

// dialRefUpdater connects a RefService client to address, authenticated with
// the shared token when it is set.
func dialRefUpdater(ctx context.Context, address, token string, repo *sdk.Repo, storage string) (*refUpdater, error) {
	var grpcOpts []grpc.DialOption
	if token != "" {
		grpcOpts = append(grpcOpts, grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token)))
	}
	conn, err := client.DialContext(ctx, address, client.WithGrpcOptions(grpcOpts))
	if err != nil {
		return nil, fmt.Errorf("dial ref service: %w", err)
	}

	rel := repo.RelativePath()
	return &refUpdater{
		repo: repo,
		repository: &gitalypb.Repository{
			StorageName:  storage,
			RelativePath: rel,
			GlRepository: "graft:" + storage + "/" + rel,
		},
		conn: conn,
		refs: gitalypb.NewRefServiceClient(conn),
	}, nil
}

// Close releases the connection.
func (r *refUpdater) Close() error {
	return r.conn.Close()
}

// Current implements greenRefs. The reference is read by its exact name: a
// revision lookup would apply git's DWIM rules, so that before the green ref
// exists a branch named like it (refs/heads/refs/graft/green/<branch>) would
// be taken for it, become the bisection baseline and the expected old value
// of every move, which then always fails.
func (r *refUpdater) Current(ctx context.Context, ref string) (string, error) {
	return r.lookupRef(ctx, ref)
}

// lookupRef returns the object ID that the reference named exactly name points
// to, or "" when there is no such reference. No DWIM rule applies: the name is
// listed as a pattern and only a reference of exactly that name is taken.
func (r *refUpdater) lookupRef(ctx context.Context, name string) (string, error) {
	if !isFullRefName(name) {
		return "", fmt.Errorf("verify: %q is not a full reference name", name)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := r.refs.ListRefs(ctx, &gitalypb.ListRefsRequest{
		Repository: r.repository,
		Patterns:   [][]byte{[]byte(name)},
	})
	if err != nil {
		return "", fmt.Errorf("list %s: %w", name, err)
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("list %s: %w", name, err)
		}
		for _, ref := range resp.GetReferences() {
			if string(ref.GetName()) == name {
				return ref.GetTarget(), nil
			}
		}
	}
}

// isFullRefName reports whether rev is a plain, full reference name such as
// refs/heads/main, without a revision suffix or pattern characters, which a
// pattern of ListRefs matches literally.
func isFullRefName(rev string) bool {
	return strings.HasPrefix(rev, "refs/") &&
		!strings.ContainsAny(rev, "^~:?*[\\ \t\n") &&
		!strings.Contains(rev, "@{") &&
		!strings.Contains(rev, "..")
}

// Move implements greenRefs.
func (r *refUpdater) Move(ctx context.Context, ref, old, next string) error {
	if next == "" {
		return errors.New("green ref move needs a new object ID")
	}
	if old == "" {
		old = zeroOIDLike(next)
	}

	stream, err := r.refs.UpdateReferences(ctx)
	if err != nil {
		return fmt.Errorf("update %s: %w", ref, err)
	}
	if err := stream.Send(&gitalypb.UpdateReferencesRequest{
		Repository: r.repository,
		Updates: []*gitalypb.UpdateReferencesRequest_Update{{
			Reference:   []byte(ref),
			OldObjectId: []byte(old),
			NewObjectId: []byte(next),
		}},
	}); err != nil {
		return fmt.Errorf("update %s: %w", ref, err)
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		return fmt.Errorf("update %s from %s to %s: %w", ref, old, next, err)
	}
	return nil
}
