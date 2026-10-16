package sdk_test

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// The fake servers below check the SDK's wire behaviour in isolation (request
// shape, error mapping, retries). integration_test.go covers every method
// end to end against a real Gitaly server.

// startServer serves the services registered by register on a loopback TCP
// listener and returns the address in the form accepted by sdk.Dial.
func startServer(t *testing.T, register func(*grpc.Server)) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := grpc.NewServer()
	register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return "tcp://" + lis.Addr().String()
}

// dialFake starts a fake server and returns an SDK client connected to it.
func dialFake(t *testing.T, register func(*grpc.Server), opts ...sdk.DialOption) *sdk.Client {
	t.Helper()

	client, err := sdk.Dial(t.Context(), startServer(t, register), opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return client
}

// fakeServices bundles the fake implementations used by most tests.
type fakeServices struct {
	objects    *fakeObjects
	commits    *fakeCommits
	operations *fakeOperations
	changesets *fakeChangesets
	derived    *fakeDerived
}

func newFakeServices() *fakeServices {
	return &fakeServices{
		objects:    &fakeObjects{},
		commits:    &fakeCommits{},
		operations: &fakeOperations{},
		changesets: &fakeChangesets{},
		derived:    &fakeDerived{},
	}
}

func (f *fakeServices) register(srv *grpc.Server) {
	gitalypb.RegisterObjectServiceServer(srv, f.objects)
	gitalypb.RegisterCommitServiceServer(srv, f.commits)
	gitalypb.RegisterOperationServiceServer(srv, f.operations)
	gitalypb.RegisterChangesetServiceServer(srv, f.changesets)
	gitalypb.RegisterDerivedDataServiceServer(srv, f.derived)
}

// fakeObjects implements ObjectService. Objects are served from a map and
// chunked by chunkSize. It can fail the first GetObjects stream after
// failAfter messages to exercise resumption.
type fakeObjects struct {
	gitalypb.UnimplementedObjectServiceServer

	mu sync.Mutex
	// objects maps object IDs to their content. Missing IDs are reported as
	// missing.
	objects map[string][]byte
	// types maps object IDs to their type. Unknown IDs are blobs.
	types map[string]gitalypb.ObjectType
	// chunkSize bounds the data per message. Zero means 1 MiB.
	chunkSize int
	// failAfter fails the first GetObjects stream with Unavailable after this
	// many messages. Zero disables the failure.
	failAfter int
	// maxObjects rejects requests for more object IDs with InvalidArgument,
	// like the server's max_objects. Zero disables the limit.
	maxObjects int
	// hideMaximum leaves the limit out of the rejection message.
	hideMaximum bool
	// getObjectsRequests records every GetObjects request.
	getObjectsRequests []*gitalypb.GetObjectsRequest
	// authorization records the authorization metadata of the last
	// GetObjects call.
	authorization string

	// tree is the list of entries of the repository's tree at revision "main",
	// with paths relative to the root.
	tree []*gitalypb.GetTreeResponse_Entry
	// getTreeRequests records every GetTree request.
	getTreeRequests []*gitalypb.GetTreeRequest
}

func (f *fakeObjects) GetObjects(req *gitalypb.GetObjectsRequest, stream gitalypb.ObjectService_GetObjectsServer) error {
	f.mu.Lock()
	f.getObjectsRequests = append(f.getObjectsRequests, req)
	call := len(f.getObjectsRequests)
	if md, ok := metadata.FromIncomingContext(stream.Context()); ok {
		if values := md.Get("authorization"); len(values) > 0 {
			f.authorization = values[0]
		}
	}
	f.mu.Unlock()

	if f.maxObjects > 0 && len(req.GetOids()) > f.maxObjects {
		if f.hideMaximum {
			return status.Errorf(codes.InvalidArgument, "too many object IDs: %d", len(req.GetOids()))
		}
		return status.Errorf(codes.InvalidArgument, "too many object IDs: %d, maximum is %d", len(req.GetOids()), f.maxObjects)
	}

	chunk := f.chunkSize
	if chunk == 0 {
		chunk = 1 << 20
	}

	sent := 0
	for _, oid := range req.GetOids() {
		content, ok := f.objects[oid]
		if !ok {
			if err := stream.Send(&gitalypb.GetObjectsResponse{Oid: oid, Missing: true}); err != nil {
				return err
			}
			continue
		}

		offset := 0
		if oid == req.GetResumeOid() {
			offset = int(req.GetResumeOffset())
		}

		for {
			end := min(offset+chunk, len(content))
			msg := &gitalypb.GetObjectsResponse{
				Oid:    oid,
				Offset: int64(offset),
				Data:   content[offset:end],
			}
			if offset == 0 {
				msg.Type = f.typeOf(oid)
				msg.Size = int64(len(content))
			}

			if f.failAfter > 0 && call == 1 && sent == f.failAfter {
				return status.Error(codes.Unavailable, "connection dropped")
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
			sent++

			offset = end
			if offset >= len(content) {
				break
			}
		}
	}

	return nil
}

func (f *fakeObjects) typeOf(oid string) gitalypb.ObjectType {
	if t, ok := f.types[oid]; ok {
		return t
	}
	return gitalypb.ObjectType_BLOB
}

func (f *fakeObjects) GetTree(req *gitalypb.GetTreeRequest, stream gitalypb.ObjectService_GetTreeServer) error {
	f.mu.Lock()
	f.getTreeRequests = append(f.getTreeRequests, req)
	f.mu.Unlock()

	prefix := ""
	if p := string(req.GetPath()); p != "" {
		prefix = p + "/"
	}

	var entries []*gitalypb.GetTreeResponse_Entry
	for _, e := range f.tree {
		if len(e.GetPath()) <= len(prefix) || string(e.GetPath()[:len(prefix)]) != prefix {
			continue
		}
		rel := e.GetPath()[len(prefix):]
		if !req.GetRecursive() && containsSlash(rel) {
			continue
		}
		entries = append(entries, &gitalypb.GetTreeResponse_Entry{
			Path: rel,
			Oid:  e.GetOid(),
			Type: e.GetType(),
			Mode: e.GetMode(),
			Size: e.GetSize(),
		})
	}

	return stream.Send(&gitalypb.GetTreeResponse{TreeOid: "tree-oid", Entries: entries})
}

func containsSlash(b []byte) bool {
	for _, c := range b {
		if c == '/' {
			return true
		}
	}
	return false
}

// fakeCommits implements CommitService for revision resolution and blob reads.
type fakeCommits struct {
	gitalypb.UnimplementedCommitServiceServer

	// revisions maps revision names to commit IDs.
	revisions map[string]string
	// files maps paths at revision "main" to their content.
	files map[string][]byte
	// dirs lists the directory paths at revision "main".
	dirs map[string]bool
	// chunkSize bounds the data per TreeEntry message. Zero means 1 MiB.
	chunkSize int
}

func (f *fakeCommits) FindCommit(_ context.Context, req *gitalypb.FindCommitRequest) (*gitalypb.FindCommitResponse, error) {
	id, ok := f.revisions[string(req.GetRevision())]
	if !ok {
		return &gitalypb.FindCommitResponse{}, nil
	}
	return &gitalypb.FindCommitResponse{Commit: &gitalypb.GitCommit{Id: id}}, nil
}

func (f *fakeCommits) TreeEntry(req *gitalypb.TreeEntryRequest, stream gitalypb.CommitService_TreeEntryServer) error {
	path := string(req.GetPath())

	if f.dirs[path] {
		return stream.Send(&gitalypb.TreeEntryResponse{Type: gitalypb.TreeEntryResponse_TREE, Oid: "dir-oid"})
	}

	content, ok := f.files[path]
	if !ok {
		return status.Errorf(codes.NotFound, "path %q not found", path)
	}

	chunk := f.chunkSize
	if chunk == 0 {
		chunk = 1 << 20
	}

	for offset := 0; ; {
		end := min(offset+chunk, len(content))
		msg := &gitalypb.TreeEntryResponse{Data: content[offset:end]}
		if offset == 0 {
			msg.Type = gitalypb.TreeEntryResponse_BLOB
			msg.Oid = "blob-oid"
			msg.Size = int64(len(content))
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
		offset = end
		if offset >= len(content) {
			return nil
		}
	}
}

// fakeOperations implements the OperationService RPCs used by the SDK.
type fakeOperations struct {
	gitalypb.UnimplementedOperationServiceServer

	mu sync.Mutex
	// commitHeaders records the headers of UserCommitFiles calls.
	commitHeaders []*gitalypb.UserCommitFilesRequestHeader
	// commitActions records the actions of the last UserCommitFiles call.
	commitActions []recordedAction
	// indexError, if set, is returned in the response of UserCommitFiles.
	indexError string
	// landRequests records every UserLand request.
	landRequests []*gitalypb.UserLandRequest
	// landResponse is returned by UserLand.
	landResponse *gitalypb.UserLandResponse
	// landErr, if set, is returned by UserLand instead of landResponse.
	landErr error
}

type recordedAction struct {
	header  *gitalypb.UserCommitFilesActionHeader
	content []byte
}

func (f *fakeOperations) UserCommitFiles(stream gitalypb.OperationService_UserCommitFilesServer) error {
	var actions []recordedAction
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		switch payload := req.GetUserCommitFilesRequestPayload().(type) {
		case *gitalypb.UserCommitFilesRequest_Header:
			f.mu.Lock()
			f.commitHeaders = append(f.commitHeaders, payload.Header)
			f.mu.Unlock()
		case *gitalypb.UserCommitFilesRequest_Action:
			switch action := payload.Action.GetUserCommitFilesActionPayload().(type) {
			case *gitalypb.UserCommitFilesAction_Header:
				actions = append(actions, recordedAction{header: action.Header})
			case *gitalypb.UserCommitFilesAction_Content:
				last := &actions[len(actions)-1]
				last.content = append(last.content, action.Content...)
			}
		}
	}

	f.mu.Lock()
	f.commitActions = actions
	indexError := f.indexError
	f.mu.Unlock()

	return stream.SendAndClose(&gitalypb.UserCommitFilesResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{CommitId: "new-commit", BranchCreated: true},
		IndexError:   indexError,
	})
}

func (f *fakeOperations) UserLand(_ context.Context, req *gitalypb.UserLandRequest) (*gitalypb.UserLandResponse, error) {
	f.mu.Lock()
	f.landRequests = append(f.landRequests, req)
	f.mu.Unlock()

	if f.landErr != nil {
		return nil, f.landErr
	}
	return f.landResponse, nil
}

// fakeChangesets implements ChangesetService over a fixed log. Changesets
// sent on live are streamed to Watch clients after the backlog. If
// dropAfter is positive, the first Watch stream fails with Unavailable after
// that many changesets.
type fakeChangesets struct {
	gitalypb.UnimplementedChangesetServiceServer

	log       []*gitalypb.Changeset
	live      chan *gitalypb.Changeset
	dropAfter int
	// watchErr, if set, is returned by every WatchChangesets call.
	watchErr error

	mu            sync.Mutex
	watchRequests []*gitalypb.WatchChangesetsRequest
}

func (f *fakeChangesets) ListChangesets(req *gitalypb.ListChangesetsRequest, stream gitalypb.ChangesetService_ListChangesetsServer) error {
	var out []*gitalypb.Changeset
	for _, cs := range f.log {
		if cs.GetSequence() > req.GetAfterSequence() {
			out = append(out, cs)
		}
	}
	if limit := int(req.GetLimit()); limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return stream.Send(&gitalypb.ListChangesetsResponse{Changesets: out})
}

func (f *fakeChangesets) WatchChangesets(req *gitalypb.WatchChangesetsRequest, stream gitalypb.ChangesetService_WatchChangesetsServer) error {
	f.mu.Lock()
	f.watchRequests = append(f.watchRequests, req)
	first := len(f.watchRequests) == 1
	f.mu.Unlock()

	if f.watchErr != nil {
		return f.watchErr
	}

	sent := 0
	for _, cs := range f.log {
		if cs.GetSequence() <= req.GetAfterSequence() {
			continue
		}
		if first && f.dropAfter > 0 && sent == f.dropAfter {
			return status.Error(codes.Unavailable, "connection dropped")
		}
		if err := stream.Send(&gitalypb.WatchChangesetsResponse{Changeset: cs}); err != nil {
			return err
		}
		sent++
	}

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case cs := <-f.live:
			if err := stream.Send(&gitalypb.WatchChangesetsResponse{Changeset: cs}); err != nil {
				return err
			}
		}
	}
}

// fakeDerived implements DerivedDataService.
type fakeDerived struct {
	gitalypb.UnimplementedDerivedDataServiceServer

	// blame is returned by Blame. Nil means the file does not exist.
	blame *gitalypb.BlameResponse
	// commits is returned by PathHistory, truncated to the request limit.
	commits []*gitalypb.GitCommit

	mu sync.Mutex
	// lastPathHistoryRequest records the last PathHistory request.
	lastPathHistoryRequest *gitalypb.PathHistoryRequest
}

func (f *fakeDerived) Blame(req *gitalypb.BlameRequest, stream gitalypb.DerivedDataService_BlameServer) error {
	if f.blame == nil {
		return status.Error(codes.NotFound, "file not found")
	}
	return stream.Send(f.blame)
}

func (f *fakeDerived) PathHistory(req *gitalypb.PathHistoryRequest, stream gitalypb.DerivedDataService_PathHistoryServer) error {
	f.mu.Lock()
	f.lastPathHistoryRequest = req
	f.mu.Unlock()

	commits := f.commits
	if limit := int(req.GetLimit()); limit > 0 && len(commits) > limit {
		commits = commits[:limit]
	}
	return stream.Send(&gitalypb.PathHistoryResponse{Commits: commits})
}
