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

// ConflictSides are the contents of the sides of a conflicting file. An absent side is nil, and so
// is a side that cannot be read: Unreadable gives the reason of each, by side ("base", "ours",
// "theirs").
type ConflictSides struct {
	Base, Ours, Theirs []byte
	Unreadable         map[string]string
}

// ConflictSides reads the sides of every file of a conflict package, by path, with GetObjects. A
// side that cannot be read costs only that side: one the package marks unavailable (the merge
// wrote it for the conflict, and the repository did not keep it), or one the repository does not
// have, is nil with its reason in Unreadable. An error is a failure to read at all.
func (r *Repo) ConflictSides(ctx context.Context, c *gitalypb.LandConflict) (map[string]ConflictSides, error) {
	var oids []string
	seen := map[string]bool{}
	for _, f := range c.GetFiles() {
		for _, s := range []*gitalypb.LandConflictSide{f.GetBase(), f.GetOurs(), f.GetTheirs()} {
			if oid := s.GetOid(); oid != "" && !seen[oid] {
				seen[oid] = true
				oids = append(oids, oid)
			}
		}
	}
	data := make(map[string][]byte, len(oids))
	missing := map[string]bool{}
	if len(oids) > 0 {
		objects, err := r.GetObjects(ctx, oids)
		if err != nil {
			return nil, err
		}
		for _, o := range objects {
			if o.Missing {
				missing[o.OID] = true
				continue
			}
			data[o.OID] = o.Data
		}
	}
	out := make(map[string]ConflictSides, len(c.GetFiles()))
	for _, f := range c.GetFiles() {
		var sides [3][]byte
		unreadable := map[string]string{}
		for i, s := range []*gitalypb.LandConflictSide{f.GetBase(), f.GetOurs(), f.GetTheirs()} {
			name := []string{"base", "ours", "theirs"}[i]
			switch {
			case s == nil:
			case s.GetOid() == "":
				unreadable[name] = s.GetUnavailable()
			case missing[s.GetOid()]:
				unreadable[name] = "the blob " + s.GetOid() + " is missing from the repository"
			default:
				sides[i] = data[s.GetOid()]
				if sides[i] == nil {
					sides[i] = []byte{}
				}
			}
		}
		cs := ConflictSides{Base: sides[0], Ours: sides[1], Theirs: sides[2]}
		if len(unreadable) > 0 {
			cs.Unreadable = unreadable
		}
		out[string(f.GetPath())] = cs
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
