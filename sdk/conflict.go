package sdk

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// MaxLandRequestBytes is the gRPC message limit of the server (4 MiB). A Land request with a
// Resolution must fit in it; Land refuses a larger one before sending it.
const MaxLandRequestBytes = 4 << 20

// LandConflict returns the conflict package of a Land call that failed on its conflicts: every
// conflicting file with its sides, its conflicting hunks and what the server did with it (see
// doc/graft/RESOLVE.md). The server sends it when the call set ResolveConflicts or Resolution.
// Resolve the files and land again with LandOptions.Resolution (see ResolutionFor).
func LandConflict(err error) (*gitalypb.LandConflict, bool) {
	return detailOf[*gitalypb.LandConflict](err)
}

// ConflictSides are the contents of the sides of a conflicting file; an absent side is nil.
type ConflictSides struct {
	Base, Ours, Theirs []byte
}

// ConflictSides reads the sides of every file of a conflict package, by path, with GetObjects.
func (r *Repo) ConflictSides(ctx context.Context, c *gitalypb.LandConflict) (map[string]ConflictSides, error) {
	var oids []string
	for _, f := range c.GetFiles() {
		for _, s := range []*gitalypb.LandConflictSide{f.GetBase(), f.GetOurs(), f.GetTheirs()} {
			if s != nil {
				oids = append(oids, s.GetOid())
			}
		}
	}
	objects, err := r.GetObjects(ctx, oids)
	if err != nil {
		return nil, err
	}
	data := make(map[string][]byte, len(objects))
	for _, o := range objects {
		if o.Missing {
			return nil, status.Errorf(codes.NotFound, "conflict side %s is missing from the repository", o.OID)
		}
		data[o.OID] = o.Data
	}
	read := func(s *gitalypb.LandConflictSide) []byte {
		if s == nil {
			return nil
		}
		if b := data[s.GetOid()]; b != nil {
			return b
		}
		return []byte{}
	}
	out := make(map[string]ConflictSides, len(c.GetFiles()))
	for _, f := range c.GetFiles() {
		out[string(f.GetPath())] = ConflictSides{Base: read(f.GetBase()), Ours: read(f.GetOurs()), Theirs: read(f.GetTheirs())}
	}
	return out, nil
}

// Resolution is the caller's resolution of the conflicts of a conflict package, for
// LandOptions.Resolution. It must resolve every conflicting path exactly once.
type Resolution struct {
	// Tip and Commit are the tip and the landed commit of the package (LandConflict.Tip and
	// LandConflict.Commit).
	Tip, Commit string
	// Files resolve the conflicting paths.
	Files []ResolvedFile
}

// ResolvedFile resolves one conflicting path: Delete it, or write Content with Mode.
type ResolvedFile struct {
	Path    string
	Delete  bool
	Mode    string
	Content []byte
}

// ResolutionFor returns an empty resolution of the conflicts of c: its tip and commit, and no
// file yet.
func ResolutionFor(c *gitalypb.LandConflict) *Resolution {
	return &Resolution{Tip: c.GetTip(), Commit: c.GetCommit()}
}

// toProto returns the resolution as the request field.
func (r *Resolution) toProto() *gitalypb.LandResolution {
	out := &gitalypb.LandResolution{Tip: r.Tip, Commit: r.Commit}
	for _, f := range r.Files {
		rf := &gitalypb.LandResolvedFile{Path: []byte(f.Path), Action: gitalypb.LandResolvedFile_ACTION_WRITE, Mode: f.Mode, Content: f.Content}
		if f.Delete {
			rf.Action, rf.Mode, rf.Content = gitalypb.LandResolvedFile_ACTION_DELETE, "", nil
		}
		out.Files = append(out.Files, rf)
	}
	return out
}

// checkLandRequestSize refuses a request that the server's 4 MiB message limit would refuse,
// with a clear error instead of a transport failure.
func checkLandRequestSize(req *gitalypb.UserLandRequest) error {
	if n := proto.Size(req); n > MaxLandRequestBytes {
		return status.Errorf(codes.InvalidArgument,
			"the land request with its resolution is %d bytes, more than the server's 4 MiB message limit: resolve fewer or smaller files per land", n)
	}
	return nil
}
