package sdk

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// ObjectType is the type of a Git object.
type ObjectType = gitalypb.ObjectType

// Object types.
const (
	ObjectBlob   = gitalypb.ObjectType_BLOB
	ObjectTree   = gitalypb.ObjectType_TREE
	ObjectCommit = gitalypb.ObjectType_COMMIT
)

// Repo is a handle to one repository on the server. Handles are cheap to
// create and safe for concurrent use.
type Repo struct {
	client       *Client
	relativePath string
}

// RelativePath returns the repository's path relative to its storage.
func (r *Repo) RelativePath() string {
	return r.relativePath
}

// repository returns a fresh protobuf repository reference for a request.
//
// Reference updates run the server's hooks, which require a GL repository
// identifier. The SDK sends the same identifier that a standalone server
// assigns to repositories without one, so that writes behave the same on
// both.
func (r *Repo) repository() *gitalypb.Repository {
	return &gitalypb.Repository{
		StorageName:  r.client.storage,
		RelativePath: r.relativePath,
		GlRepository: "graft:" + r.client.storage + "/" + r.relativePath,
	}
}

// ResolveRevision resolves a revision (branch, tag, object ID or any
// gitrevisions expression) to the full object ID of a commit. It returns a
// NotFound error if the revision does not name a commit.
func (r *Repo) ResolveRevision(ctx context.Context, rev string) (string, error) {
	if rev == "" {
		return "", status.Error(codes.InvalidArgument, "revision is required")
	}

	resp, err := r.client.commits.FindCommit(ctx, &gitalypb.FindCommitRequest{
		Repository: r.repository(),
		Revision:   []byte(rev),
	})
	if err != nil {
		return "", err
	}
	if resp.GetCommit() == nil {
		return "", status.Errorf(codes.NotFound, "revision %q not found", rev)
	}

	return resp.GetCommit().GetId(), nil
}

// ReadFile returns the contents of the blob at path in the tree of rev. It
// fails with NotFound if the path does not exist and with FailedPrecondition
// if it names a directory or a submodule.
func (r *Repo) ReadFile(ctx context.Context, rev, path string) ([]byte, error) {
	if rev == "" || path == "" {
		return nil, status.Error(codes.InvalidArgument, "revision and path are required")
	}

	stream, err := r.client.commits.TreeEntry(ctx, &gitalypb.TreeEntryRequest{
		Repository: r.repository(),
		Revision:   []byte(rev),
		Path:       []byte(path),
	})
	if err != nil {
		return nil, err
	}

	var data []byte
	first := true
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return data, nil
		}
		if err != nil {
			return nil, err
		}

		if first {
			first = false
			if msg.GetType() != gitalypb.TreeEntryResponse_BLOB {
				return nil, status.Errorf(codes.FailedPrecondition, "%q at %q is not a file", path, rev)
			}
		}

		data = append(data, msg.GetData()...)
	}
}

// TreeEntry is one entry of a tree returned by ListTree.
type TreeEntry struct {
	// Path is the entry path relative to the listed tree.
	Path string
	// OID is the object ID of the entry.
	OID string
	// Type is the object type: blob, tree or commit (submodule).
	Type ObjectType
	// Mode is the Git file mode, for example 0100644.
	Mode int32
	// Size is the blob size in bytes. Zero for trees and submodules.
	Size int64
}

// IsDir reports whether the entry is a subdirectory.
func (e TreeEntry) IsDir() bool {
	return e.Type == ObjectTree
}

// ListTree lists the entries of the directory at path in the tree of rev.
// An empty path lists the root tree. If recursive is true, the whole subtree
// is listed depth-first, with paths relative to the listed directory. Blob
// sizes are always included.
func (r *Repo) ListTree(ctx context.Context, rev, path string, recursive bool) ([]TreeEntry, error) {
	if rev == "" {
		return nil, status.Error(codes.InvalidArgument, "revision is required")
	}

	stream, err := r.client.objects.GetTree(ctx, &gitalypb.GetTreeRequest{
		Repository:   r.repository(),
		Revision:     []byte(rev),
		Path:         []byte(path),
		Recursive:    recursive,
		IncludeSizes: true,
	})
	if err != nil {
		return nil, err
	}

	var entries []TreeEntry
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}

		for _, e := range msg.GetEntries() {
			entries = append(entries, TreeEntry{
				Path: string(e.GetPath()),
				OID:  e.GetOid(),
				Type: e.GetType(),
				Mode: e.GetMode(),
				Size: e.GetSize(),
			})
		}
	}
}
