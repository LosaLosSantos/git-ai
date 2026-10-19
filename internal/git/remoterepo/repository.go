package remoterepo

import (
	"context"
	"fmt"
	"sync"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc"
)

// Repo represents a Git repository on a different Gitaly storage
type Repo struct {
	*gitalypb.Repository
	conn *grpc.ClientConn

	detectObjectHashOnce sync.Once
	objectHash           git.ObjectHash
	objectHashErr        error
}

// New creates a new remote Repository from its protobuf representation.
func New(ctx context.Context, repo *gitalypb.Repository, pool *client.Pool) (*Repo, error) {
	server, err := storage.ExtractGitalyServer(ctx, repo.GetStorageName())
	if err != nil {
		return nil, fmt.Errorf("remote repository: %w", err)
	}

	cc, err := pool.Dial(ctx, server.Address, server.Token)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	return &Repo{
		Repository: repo,
		conn:       cc,
	}, nil
}

// ObjectHash detects the object hash used by this particular repository.
func (rr *Repo) ObjectHash(ctx context.Context) (git.ObjectHash, error) {
	rr.detectObjectHashOnce.Do(func() {
		rr.objectHash, rr.objectHashErr = func() (git.ObjectHash, error) {
			client := gitalypb.NewRepositoryServiceClient(rr.conn)

			response, err := client.ObjectFormat(ctx, &gitalypb.ObjectFormatRequest{
				Repository: rr.Repository,
			})
			if err != nil {
				return git.ObjectHash{}, err
			}

			return git.ObjectHashByProto(response.GetFormat())
		}()
	})

	return rr.objectHash, rr.objectHashErr
}

// ResolveRevision will dial to the remote repository and attempt to resolve the
// revision string via the gRPC interface.
func (rr *Repo) ResolveRevision(ctx context.Context, revision git.Revision) (git.ObjectID, error) {
	cli := gitalypb.NewCommitServiceClient(rr.conn)
	resp, err := cli.FindCommit(ctx, &gitalypb.FindCommitRequest{
		Repository: rr.Repository,
		Revision:   []byte(revision.String()),
	})
	if err != nil {
		return "", err
	}

	oidHex := resp.GetCommit().GetId()
	if oidHex == "" {
		return "", git.ErrReferenceNotFound
	}

	objectHash, err := rr.ObjectHash(ctx)
	if err != nil {
		return "", fmt.Errorf("detecting object hash: %w", err)
	}

	oid, err := objectHash.FromHex(oidHex)
	if err != nil {
		return "", fmt.Errorf("parsing object ID: %w", err)
	}

	return oid, nil
}

// ResolveBranch will dial to the remote repository and resolve the branch named branch (without
// refs/heads/) by its exact reference name to the commit it points to. Unlike ResolveRevision,
// whose revision git resolves with its DWIM rules, a missing branch never resolves through
// refs/tags/refs/heads/<branch> or another reference: it returns git.ErrReferenceNotFound.
func (rr *Repo) ResolveBranch(ctx context.Context, branch string) (git.ObjectID, error) {
	resp, err := gitalypb.NewRefServiceClient(rr.conn).FindBranch(ctx, &gitalypb.FindBranchRequest{
		Repository: rr.Repository,
		Name:       []byte(branch),
	})
	if err != nil {
		return "", fmt.Errorf("find branch: %w", err)
	}

	oidHex := resp.GetBranch().GetTargetCommit().GetId()
	if oidHex == "" {
		return "", git.ErrReferenceNotFound
	}

	objectHash, err := rr.ObjectHash(ctx)
	if err != nil {
		return "", fmt.Errorf("detecting object hash: %w", err)
	}

	oid, err := objectHash.FromHex(oidHex)
	if err != nil {
		return "", fmt.Errorf("parsing object ID: %w", err)
	}

	return oid, nil
}

// HasBranches will dial to the remote repository and check whether the repository has any branches.
func (rr *Repo) HasBranches(ctx context.Context) (bool, error) {
	resp, err := gitalypb.NewRepositoryServiceClient(rr.conn).HasLocalBranches(
		ctx, &gitalypb.HasLocalBranchesRequest{Repository: rr.Repository})
	if err != nil {
		return false, fmt.Errorf("has local branches: %w", err)
	}

	return resp.GetValue(), nil
}

// GetDefaultBranch returns the default branch for the remote repository. It does so by invoking
// `FindDefaultBranchName()`, which itself is a wrapper around `localrepo.GetDefaultBranch()`.
// Semantics of this function thus match the localrepo semantics.
func (rr *Repo) GetDefaultBranch(ctx context.Context) (git.ReferenceName, error) {
	resp, err := gitalypb.NewRefServiceClient(rr.conn).FindDefaultBranchName(
		ctx, &gitalypb.FindDefaultBranchNameRequest{Repository: rr.Repository})
	if err != nil {
		return "", err
	}

	return git.ReferenceName(resp.GetName()), nil
}

// HeadReference returns the reference that HEAD points to for the remote
// repository.
func (rr *Repo) HeadReference(ctx context.Context) (git.ReferenceName, error) {
	resp, err := gitalypb.NewRefServiceClient(rr.conn).FindDefaultBranchName(
		ctx, &gitalypb.FindDefaultBranchNameRequest{
			Repository: rr.Repository,
			HeadOnly:   true,
		})
	if err != nil {
		return "", err
	}

	return git.ReferenceName(resp.GetName()), nil
}
