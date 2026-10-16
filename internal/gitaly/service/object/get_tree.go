package object

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	// getTreeEntriesPerMessage is the maximum number of entries sent in one GetTreeResponse.
	getTreeEntriesPerMessage = 1000

	// fileModeTree is the raw Git file mode of a subtree.
	fileModeTree = 0o040000
	// fileModeCommit is the raw Git file mode of a submodule (gitlink).
	fileModeCommit = 0o160000
)

// GetTree lists the entries of a tree. See the ObjectService documentation in proto/object.proto.
func (s *server) GetTree(req *gitalypb.GetTreeRequest, stream gitalypb.ObjectService_GetTreeServer) error {
	ctx := stream.Context()

	if err := s.deps.GetLocator().ValidateRepository(ctx, req.GetRepository()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	repo := s.deps.GetRepositoryFactory().Build(req.GetRepository())

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return structerr.NewInternal("detecting object hash: %w", err)
	}

	if err := validateGetTreeRequest(req, objectHash); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	objectReader, cancel, err := s.deps.GetCatfileCache().ObjectReader(ctx, repo)
	if err != nil {
		return structerr.NewInternal("creating object reader: %w", err)
	}
	defer cancel()

	treeObject, err := resolveTree(ctx, objectReader, req)
	if err != nil {
		return err
	}

	walker := &treeWalker{
		ctx:        ctx,
		stream:     stream,
		objectHash: objectHash,
		reader:     objectReader,
		recursive:  req.GetRecursive(),
		treeOID:    treeObject.oid,
	}

	if req.GetIncludeSizes() {
		infoReader, cancel, err := s.deps.GetCatfileCache().ObjectInfoReader(ctx, repo)
		if err != nil {
			return structerr.NewInternal("creating object info reader: %w", err)
		}
		defer cancel()

		walker.infoReader = infoReader
	}

	if err := walker.walkTree(treeObject.data, ""); err != nil {
		return err
	}

	return walker.flush(true)
}

// validateGetTreeRequest checks that exactly one way of selecting the tree is used and that the
// selection is well-formed.
func validateGetTreeRequest(req *gitalypb.GetTreeRequest, objectHash git.ObjectHash) error {
	hasTreeOID := req.GetTreeOid() != ""
	hasRevision := len(req.GetRevision()) > 0

	switch {
	case hasTreeOID && hasRevision:
		return errors.New("tree_oid and revision are mutually exclusive")
	case hasTreeOID:
		if _, err := objectHash.FromHex(req.GetTreeOid()); err != nil {
			return fmt.Errorf("invalid tree_oid %q: %w", req.GetTreeOid(), err)
		}
	case hasRevision:
		if err := git.ValidateRevision(req.GetRevision()); err != nil {
			return fmt.Errorf("invalid revision: %w", err)
		}
		if bytes.Contains(req.GetRevision(), []byte(":")) {
			return errors.New("revision must not contain ':'")
		}
		if _, err := normalizeTreePath(req.GetPath()); err != nil {
			return err
		}
	default:
		return errors.New("either tree_oid or revision must be set")
	}

	return nil
}

// normalizeTreePath strips leading and trailing slashes from a user supplied path and rejects
// paths that are not plain relative paths inside the tree.
func normalizeTreePath(rawPath []byte) (string, error) {
	path := strings.Trim(string(rawPath), "/")
	if path == "" {
		return "", nil
	}

	for _, component := range strings.Split(path, "/") {
		switch component {
		case "", ".", "..":
			return "", fmt.Errorf("invalid path component %q in path %q", component, rawPath)
		}
		if strings.ContainsRune(component, 0) {
			return "", fmt.Errorf("path %q contains a NUL byte", rawPath)
		}
	}

	return path, nil
}

// resolvedTree is the tree which is about to be listed.
type resolvedTree struct {
	oid  string
	data []byte
}

// resolveTree looks up the tree selected by the request and reads its raw contents.
func resolveTree(ctx context.Context, objectReader catfile.ObjectContentReader, req *gitalypb.GetTreeRequest) (resolvedTree, error) {
	var revision git.Revision
	if req.GetTreeOid() != "" {
		revision = git.Revision(req.GetTreeOid())
	} else {
		path, err := normalizeTreePath(req.GetPath())
		if err != nil {
			return resolvedTree{}, structerr.NewInvalidArgument("%w", err)
		}
		revision = git.Revision(string(req.GetRevision()) + ":" + path)
	}

	object, err := objectReader.Object(ctx, revision)
	if err != nil {
		var notFound catfile.NotFoundError
		if errors.As(err, &notFound) {
			if req.GetTreeOid() != "" {
				return resolvedTree{}, structerr.NewNotFound("tree not found: %s", req.GetTreeOid())
			}
			return resolvedTree{}, structerr.NewNotFound("tree not found: %q at path %q", req.GetRevision(), req.GetPath())
		}

		return resolvedTree{}, structerr.NewInternal("reading tree: %w", err)
	}

	if object.Type != "tree" {
		if _, err := io.Copy(io.Discard, object); err != nil {
			return resolvedTree{}, structerr.NewInternal("discarding object: %w", err)
		}
		return resolvedTree{}, structerr.NewInvalidArgument("object %s is a %s, not a tree", object.Oid, object.Type)
	}

	data, err := io.ReadAll(object)
	if err != nil {
		return resolvedTree{}, structerr.NewInternal("reading tree data: %w", err)
	}

	return resolvedTree{oid: object.Oid.String(), data: data}, nil
}

// rawTreeEntry is one entry of a tree object as stored by Git.
type rawTreeEntry struct {
	name    string
	oid     git.ObjectID
	mode    int64
	objType gitalypb.ObjectType
}

// parseTreeEntries parses the raw contents of a tree object. Entries keep the order in which Git
// stores them, which is the canonical order used by git-ls-tree(1).
func parseTreeEntries(objectHash git.ObjectHash, data []byte) ([]rawTreeEntry, error) {
	oidSize := objectHash.Hash().Size()

	var entries []rawTreeEntry
	for len(data) > 0 {
		space := bytes.IndexByte(data, ' ')
		if space <= 0 {
			return nil, errors.New("malformed tree: missing mode")
		}

		mode, err := strconv.ParseInt(string(data[:space]), 8, 64)
		if err != nil {
			return nil, fmt.Errorf("malformed tree: invalid mode: %w", err)
		}
		data = data[space+1:]

		nul := bytes.IndexByte(data, 0)
		if nul <= 0 {
			return nil, errors.New("malformed tree: missing name")
		}
		name := string(data[:nul])
		data = data[nul+1:]

		if len(data) < oidSize {
			return nil, errors.New("malformed tree: truncated object ID")
		}
		oid := git.ObjectID(hex.EncodeToString(data[:oidSize]))
		data = data[oidSize:]

		entries = append(entries, rawTreeEntry{
			name:    name,
			oid:     oid,
			mode:    mode,
			objType: entryObjectType(mode),
		})
	}

	return entries, nil
}

// entryObjectType derives the object type of a tree entry from its file mode.
func entryObjectType(mode int64) gitalypb.ObjectType {
	switch mode {
	case fileModeTree:
		return gitalypb.ObjectType_TREE
	case fileModeCommit:
		return gitalypb.ObjectType_COMMIT
	default:
		return gitalypb.ObjectType_BLOB
	}
}

// treeWalker lists tree entries and sends them in batches of getTreeEntriesPerMessage entries.
type treeWalker struct {
	ctx        context.Context
	stream     gitalypb.ObjectService_GetTreeServer
	objectHash git.ObjectHash
	reader     catfile.ObjectContentReader
	// infoReader is only set when sizes are requested.
	infoReader catfile.ObjectInfoReader
	recursive  bool
	treeOID    string

	pending    []*gitalypb.GetTreeResponse_Entry
	headerSent bool
}

// walkTree adds the entries of the tree with the given raw contents. Entries are visited in depth
// first order: every subtree directly follows its own entry when the listing is recursive.
// Paths are relative to the listed tree and use prefix as their directory.
func (w *treeWalker) walkTree(data []byte, prefix string) error {
	entries, err := parseTreeEntries(w.objectHash, data)
	if err != nil {
		return structerr.NewInternal("parsing tree: %w", err)
	}

	for _, entry := range entries {
		path := prefix + entry.name

		if err := w.add(&gitalypb.GetTreeResponse_Entry{
			Path: []byte(path),
			Oid:  entry.oid.String(),
			Type: entry.objType,
			Mode: int32(entry.mode),
		}); err != nil {
			return err
		}

		if !w.recursive || entry.objType != gitalypb.ObjectType_TREE {
			continue
		}

		subtreeData, err := w.readSubtree(entry.oid)
		if err != nil {
			return err
		}

		if err := w.walkTree(subtreeData, path+"/"); err != nil {
			return err
		}
	}

	return nil
}

// readSubtree reads the raw contents of a subtree. The object reader is fully drained before this
// function returns so that the walker can keep using it.
func (w *treeWalker) readSubtree(oid git.ObjectID) ([]byte, error) {
	object, err := w.reader.Object(w.ctx, git.Revision(oid))
	if err != nil {
		var notFound catfile.NotFoundError
		if errors.As(err, &notFound) {
			return nil, structerr.NewInternal("subtree %s is missing", oid)
		}
		return nil, structerr.NewInternal("reading subtree %s: %w", oid, err)
	}

	if object.Type != "tree" {
		if _, err := io.Copy(io.Discard, object); err != nil {
			return nil, structerr.NewInternal("discarding object: %w", err)
		}
		return nil, structerr.NewInternal("tree entry %s is a %s, not a tree", oid, object.Type)
	}

	data, err := io.ReadAll(object)
	if err != nil {
		return nil, structerr.NewInternal("reading subtree %s: %w", oid, err)
	}

	return data, nil
}

// add queues an entry and sends a batch once it is full.
func (w *treeWalker) add(entry *gitalypb.GetTreeResponse_Entry) error {
	w.pending = append(w.pending, entry)

	if len(w.pending) < getTreeEntriesPerMessage {
		return nil
	}

	return w.flush(false)
}

// flush resolves the sizes of the pending entries if requested and sends them. The very first
// message is always sent, even if it carries no entries, because it holds the tree OID. A
// non-final flush with nothing pending does nothing.
func (w *treeWalker) flush(final bool) error {
	if len(w.pending) == 0 && (w.headerSent || !final) {
		return nil
	}

	if w.infoReader != nil {
		if err := w.resolveSizes(); err != nil {
			return err
		}
	}

	response := &gitalypb.GetTreeResponse{Entries: w.pending}
	if !w.headerSent {
		response.TreeOid = w.treeOID
		w.headerSent = true
	}

	w.pending = nil

	if err := w.stream.Send(response); err != nil {
		return structerr.NewInternal("sending tree entries: %w", err)
	}

	return nil
}

// resolveSizes sets the size of every blob entry in the pending batch. The info requests are
// batched through a single queue.
func (w *treeWalker) resolveSizes() error {
	queue, finish, err := w.infoReader.Queue(w.ctx)
	if err != nil {
		return structerr.NewInternal("creating info queue: %w", err)
	}
	defer finish()

	blobs := make([]*gitalypb.GetTreeResponse_Entry, 0, len(w.pending))
	for _, entry := range w.pending {
		if entry.GetType() != gitalypb.ObjectType_BLOB {
			continue
		}

		if err := queue.RequestInfo(w.ctx, git.Revision(entry.GetOid())); err != nil {
			return structerr.NewInternal("requesting info for %s: %w", entry.GetOid(), err)
		}
		blobs = append(blobs, entry)
	}

	if len(blobs) == 0 {
		return nil
	}

	if err := queue.Flush(w.ctx); err != nil {
		return structerr.NewInternal("flushing info requests: %w", err)
	}

	for _, entry := range blobs {
		info, err := queue.ReadInfo(w.ctx)
		if err != nil {
			return structerr.NewInternal("reading info for %s: %w", entry.GetOid(), err)
		}

		entry.Size = info.Size
	}

	return nil
}
