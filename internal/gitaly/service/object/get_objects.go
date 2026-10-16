package object

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// GetObjects streams the raw contents of the requested objects in request
// order. See the ObjectService documentation in proto/object.proto.
func (s *server) GetObjects(req *gitalypb.GetObjectsRequest, stream gitalypb.ObjectService_GetObjectsServer) error {
	ctx := stream.Context()

	if err := s.deps.GetLocator().ValidateRepository(ctx, req.GetRepository()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	repo := s.deps.GetRepositoryFactory().Build(req.GetRepository())

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return structerr.NewInternal("detecting object hash: %w", err)
	}

	cfg := s.deps.GetCfg().Graft.ObjectService

	oids, err := validateGetObjectsRequest(req, objectHash, cfg.MaxObjectsOrDefault())
	if err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	objectReader, cancel, err := s.deps.GetCatfileCache().ObjectReader(ctx, repo)
	if err != nil {
		return structerr.NewInternal("creating object reader: %w", err)
	}
	defer cancel()

	return sendGetObjects(ctx, stream, objectReader, oids, req.GetResumeOid(), req.GetResumeOffset(), cfg.ChunkSizeOrDefault())
}

// validateGetObjectsRequest checks the request and returns the requested object IDs with duplicates
// removed, keeping the position of the first occurrence of every ID.
func validateGetObjectsRequest(req *gitalypb.GetObjectsRequest, objectHash git.ObjectHash, maxObjects int) ([]git.ObjectID, error) {
	if len(req.GetOids()) == 0 {
		return nil, errors.New("empty object IDs")
	}

	if len(req.GetOids()) > maxObjects {
		return nil, fmt.Errorf("too many object IDs: %d, maximum is %d", len(req.GetOids()), maxObjects)
	}

	seen := make(map[git.ObjectID]struct{}, len(req.GetOids()))
	oids := make([]git.ObjectID, 0, len(req.GetOids()))

	for _, rawOID := range req.GetOids() {
		oid, err := objectHash.FromHex(rawOID)
		if err != nil {
			return nil, fmt.Errorf("invalid object ID %q: %w", rawOID, err)
		}

		if _, ok := seen[oid]; ok {
			continue
		}

		seen[oid] = struct{}{}
		oids = append(oids, oid)
	}

	resumeOID := req.GetResumeOid()
	switch {
	case resumeOID == "" && req.GetResumeOffset() != 0:
		return nil, errors.New("resume_offset requires resume_oid")
	case resumeOID == "":
		return oids, nil
	case req.GetResumeOffset() < 0:
		return nil, fmt.Errorf("negative resume_offset: %d", req.GetResumeOffset())
	}

	if _, err := objectHash.FromHex(resumeOID); err != nil {
		return nil, fmt.Errorf("invalid resume_oid %q: %w", resumeOID, err)
	}

	if _, ok := seen[git.ObjectID(resumeOID)]; !ok {
		return nil, fmt.Errorf("resume_oid %q is not one of the requested object IDs", resumeOID)
	}

	return oids, nil
}

// sendGetObjects requests all objects in one batch from git-cat-file(1) and then sends them in
// request order, split into chunks of at most chunkSize bytes.
func sendGetObjects(
	ctx context.Context,
	stream gitalypb.ObjectService_GetObjectsServer,
	objectReader catfile.ObjectContentReader,
	oids []git.ObjectID,
	resumeOID string,
	resumeOffset int64,
	chunkSize int,
) error {
	queue, finish, err := objectReader.Queue(ctx)
	if err != nil {
		return structerr.NewInternal("creating object queue: %w", err)
	}
	defer finish()

	for _, oid := range oids {
		if err := queue.RequestObject(ctx, git.Revision(oid)); err != nil {
			return structerr.NewInternal("requesting object %s: %w", oid, err)
		}
	}

	if err := queue.Flush(ctx); err != nil {
		return structerr.NewInternal("flushing object requests: %w", err)
	}

	for _, oid := range oids {
		var offset int64
		if oid.String() == resumeOID {
			offset = resumeOffset
		}

		if err := sendObject(ctx, stream, queue, oid, offset, chunkSize); err != nil {
			return err
		}
	}

	return nil
}

// sendObject reads the next requested object from the queue and streams it, starting at offset.
func sendObject(
	ctx context.Context,
	stream gitalypb.ObjectService_GetObjectsServer,
	queue catfile.Queue,
	oid git.ObjectID,
	offset int64,
	chunkSize int,
) error {
	object, err := queue.ReadObject(ctx)
	if err != nil {
		var notFound catfile.NotFoundError
		if errors.As(err, &notFound) {
			if err := stream.Send(&gitalypb.GetObjectsResponse{Oid: oid.String(), Missing: true}); err != nil {
				return structerr.NewInternal("send missing object: %w", err)
			}
			return nil
		}

		return structerr.NewInternal("reading object %s: %w", oid, err)
	}

	objectType, err := protoObjectType(object.Type)
	if err != nil {
		return structerr.NewInternal("object %s: %w", oid, err)
	}

	if offset < 0 || offset > object.Size {
		return structerr.NewInvalidArgument("resume_offset %d out of range for object %s of size %d", offset, oid, object.Size)
	}

	if offset > 0 {
		if _, err := io.CopyN(io.Discard, object, offset); err != nil {
			return structerr.NewInternal("skipping to resume offset of object %s: %w", oid, err)
		}
	}

	// The first message of every object carries its type and size. The loop always sends at
	// least one message, so that empty objects are reported too.
	first := true
	position := offset
	for first || position < object.Size {
		length := min(int64(chunkSize), object.Size-position)

		data := make([]byte, length)
		if _, err := io.ReadFull(object, data); err != nil {
			return structerr.NewInternal("reading data of object %s: %w", oid, err)
		}

		response := &gitalypb.GetObjectsResponse{
			Oid:    oid.String(),
			Offset: position,
			Data:   data,
		}

		if first {
			response.Type = objectType
			response.Size = object.Size
			first = false
		}

		if err := stream.Send(response); err != nil {
			return structerr.NewInternal("send object %s: %w", oid, err)
		}

		position += length
	}

	// The queue only considers an object consumed once its reader reports EOF, which the
	// exact-length reads above never trigger. Drain the object so that the next one can be read.
	if _, err := io.Copy(io.Discard, object); err != nil {
		return structerr.NewInternal("finishing object %s: %w", oid, err)
	}

	return nil
}

// protoObjectType maps the type name reported by git-cat-file(1) to its protobuf representation.
func protoObjectType(objectType string) (gitalypb.ObjectType, error) {
	switch objectType {
	case "commit":
		return gitalypb.ObjectType_COMMIT, nil
	case "blob":
		return gitalypb.ObjectType_BLOB, nil
	case "tree":
		return gitalypb.ObjectType_TREE, nil
	case "tag":
		return gitalypb.ObjectType_TAG, nil
	default:
		return gitalypb.ObjectType_UNKNOWN, fmt.Errorf("unknown object type %q", objectType)
	}
}
