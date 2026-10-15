package requestdefaults

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/feder-cr/open_scion/internal/graft/identity"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

func agentContext(t *testing.T, name string) context.Context {
	t.Helper()
	return identity.NewContext(testhelper.Context(t), identity.Identity{Name: name, Source: identity.SourceAgent})
}

// sharedTokenContext returns a context of a caller authenticated with the
// shared auth.token. Such a caller is trusted, so its identity fields are only
// filled when empty.
func sharedTokenContext(t *testing.T) context.Context {
	t.Helper()
	return identity.NewContext(testhelper.Context(t), identity.Identity{Name: "shared", Source: identity.SourceSharedToken})
}

// runUnary runs the unary interceptor with a handler that captures the request
// it receives.
func runUnary(t *testing.T, ctx context.Context, req proto.Message) (proto.Message, error) {
	t.Helper()

	var handled proto.Message
	_, err := UnaryServerInterceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: "/gitaly.OperationService/UserCreateBranch"},
		func(_ context.Context, r any) (any, error) {
			handled = r.(proto.Message)
			return nil, nil
		})

	return handled, err
}

func TestUnaryServerInterceptor(t *testing.T) {
	t.Parallel()

	repo := func() *gitalypb.Repository {
		return &gitalypb.Repository{StorageName: "default", RelativePath: "@hashed/ab/cd/repo.git"}
	}

	testCases := []struct {
		desc  string
		ctx   func(t *testing.T) context.Context
		req   func() *gitalypb.UserCreateBranchRequest
		check func(t *testing.T, req *gitalypb.UserCreateBranchRequest)
	}{
		{
			desc: "missing user is not created",
			ctx:  func(t *testing.T) context.Context { return agentContext(t, "agent-a") },
			req: func() *gitalypb.UserCreateBranchRequest {
				return &gitalypb.UserCreateBranchRequest{Repository: repo(), BranchName: []byte("main")}
			},
			check: func(t *testing.T, req *gitalypb.UserCreateBranchRequest) {
				// A User without a name or an email would make the handlers write
				// commits and tags with an empty identity, so none is created.
				require.Nil(t, req.GetUser())
			},
		},
		{
			desc: "empty gl_id and gl_username of a present user are filled",
			ctx:  func(t *testing.T) context.Context { return agentContext(t, "agent-a") },
			req: func() *gitalypb.UserCreateBranchRequest {
				return &gitalypb.UserCreateBranchRequest{
					Repository: repo(),
					User:       &gitalypb.User{Name: []byte("Ada"), Email: []byte("ada@example.com")},
				}
			},
			check: func(t *testing.T, req *gitalypb.UserCreateBranchRequest) {
				require.Equal(t, "agent-a", req.GetUser().GetGlId())
				require.Equal(t, "agent-a", req.GetUser().GetGlUsername())
				require.Equal(t, "Ada", string(req.GetUser().GetName()))
				require.Equal(t, "ada@example.com", string(req.GetUser().GetEmail()))
			},
		},
		{
			desc: "non-empty user values of a shared-token caller are never overwritten",
			ctx:  sharedTokenContext,
			req: func() *gitalypb.UserCreateBranchRequest {
				return &gitalypb.UserCreateBranchRequest{
					Repository: repo(),
					User:       &gitalypb.User{GlId: "user-42", GlUsername: "ada"},
				}
			},
			check: func(t *testing.T, req *gitalypb.UserCreateBranchRequest) {
				require.Equal(t, "user-42", req.GetUser().GetGlId())
				require.Equal(t, "ada", req.GetUser().GetGlUsername())
			},
		},
		{
			desc: "partially set user of a shared-token caller only fills the empty field",
			ctx:  sharedTokenContext,
			req: func() *gitalypb.UserCreateBranchRequest {
				return &gitalypb.UserCreateBranchRequest{
					Repository: repo(),
					User:       &gitalypb.User{GlUsername: "ada"},
				}
			},
			check: func(t *testing.T, req *gitalypb.UserCreateBranchRequest) {
				require.Equal(t, "shared", req.GetUser().GetGlId())
				require.Equal(t, "ada", req.GetUser().GetGlUsername())
			},
		},
		{
			desc: "anonymous without identity",
			ctx:  func(t *testing.T) context.Context { return testhelper.Context(t) },
			req: func() *gitalypb.UserCreateBranchRequest {
				return &gitalypb.UserCreateBranchRequest{Repository: repo(), User: &gitalypb.User{}}
			},
			check: func(t *testing.T, req *gitalypb.UserCreateBranchRequest) {
				require.Equal(t, "anonymous", req.GetUser().GetGlId())
				require.Equal(t, "anonymous", req.GetUser().GetGlUsername())
			},
		},
		{
			desc: "empty gl_repository is derived from the target repository",
			ctx:  func(t *testing.T) context.Context { return agentContext(t, "agent-a") },
			req: func() *gitalypb.UserCreateBranchRequest {
				return &gitalypb.UserCreateBranchRequest{Repository: repo()}
			},
			check: func(t *testing.T, req *gitalypb.UserCreateBranchRequest) {
				require.Equal(t, "graft:default/@hashed/ab/cd/repo.git", req.GetRepository().GetGlRepository())
			},
		},
		{
			desc: "non-empty gl_repository is kept",
			ctx:  func(t *testing.T) context.Context { return agentContext(t, "agent-a") },
			req: func() *gitalypb.UserCreateBranchRequest {
				r := repo()
				r.GlRepository = "project-7"
				return &gitalypb.UserCreateBranchRequest{Repository: r}
			},
			check: func(t *testing.T, req *gitalypb.UserCreateBranchRequest) {
				require.Equal(t, "project-7", req.GetRepository().GetGlRepository())
			},
		},
		{
			desc: "repository without storage is left alone",
			ctx:  func(t *testing.T) context.Context { return agentContext(t, "agent-a") },
			req: func() *gitalypb.UserCreateBranchRequest {
				return &gitalypb.UserCreateBranchRequest{Repository: &gitalypb.Repository{RelativePath: "repo.git"}}
			},
			check: func(t *testing.T, req *gitalypb.UserCreateBranchRequest) {
				require.Empty(t, req.GetRepository().GetGlRepository())
			},
		},
		{
			desc: "repository without relative path is left alone",
			ctx:  func(t *testing.T) context.Context { return agentContext(t, "agent-a") },
			req: func() *gitalypb.UserCreateBranchRequest {
				return &gitalypb.UserCreateBranchRequest{Repository: &gitalypb.Repository{StorageName: "default"}}
			},
			check: func(t *testing.T, req *gitalypb.UserCreateBranchRequest) {
				require.Empty(t, req.GetRepository().GetGlRepository())
			},
		},
		{
			desc: "missing repository is not created",
			ctx:  func(t *testing.T) context.Context { return agentContext(t, "agent-a") },
			req: func() *gitalypb.UserCreateBranchRequest {
				return &gitalypb.UserCreateBranchRequest{BranchName: []byte("main")}
			},
			check: func(t *testing.T, req *gitalypb.UserCreateBranchRequest) {
				require.Nil(t, req.GetRepository())
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			req := tc.req()
			handled, err := runUnary(t, tc.ctx(t), req)
			require.NoError(t, err)
			tc.check(t, handled.(*gitalypb.UserCreateBranchRequest))
		})
	}
}

func TestUnaryServerInterceptor_NestedHeaderUnset(t *testing.T) {
	t.Parallel()

	// The user and repository of UserCommitFiles live in the nested header
	// message. A user the client did not send is never created, and only the
	// top-level repository field gets a gl_repository.
	req := &gitalypb.UserCommitFilesRequest{
		UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{
			Header: &gitalypb.UserCommitFilesRequestHeader{
				Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"},
				BranchName: []byte("main"),
			},
		},
	}

	handled, err := runUnary(t, agentContext(t, "agent-a"), req)
	require.NoError(t, err)

	header := handled.(*gitalypb.UserCommitFilesRequest).GetHeader()
	require.Nil(t, header.GetUser())
	require.Empty(t, header.GetRepository().GetGlRepository())
	require.Equal(t, []byte("main"), header.GetBranchName())
}

// nestedUserRequests returns, for every RPC whose gitaly.User lives in a header
// oneof, a request carrying user and a function returning the user the
// handler sees.
func nestedUserRequests(user *gitalypb.User) map[string]struct {
	req     proto.Message
	getUser func(proto.Message) *gitalypb.User
} {
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"}

	return map[string]struct {
		req     proto.Message
		getUser func(proto.Message) *gitalypb.User
	}{
		"UserCommitFiles": {
			req: &gitalypb.UserCommitFilesRequest{
				UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{
					Header: &gitalypb.UserCommitFilesRequestHeader{Repository: repo, User: proto.Clone(user).(*gitalypb.User), BranchName: []byte("main")},
				},
			},
			getUser: func(m proto.Message) *gitalypb.User {
				return m.(*gitalypb.UserCommitFilesRequest).GetHeader().GetUser()
			},
		},
		"UserApplyPatch": {
			req: &gitalypb.UserApplyPatchRequest{
				UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Header_{
					Header: &gitalypb.UserApplyPatchRequest_Header{Repository: repo, User: proto.Clone(user).(*gitalypb.User), TargetBranch: []byte("main")},
				},
			},
			getUser: func(m proto.Message) *gitalypb.User {
				return m.(*gitalypb.UserApplyPatchRequest).GetHeader().GetUser()
			},
		},
		"UserRebaseConfirmable": {
			req: &gitalypb.UserRebaseConfirmableRequest{
				UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Header_{
					Header: &gitalypb.UserRebaseConfirmableRequest_Header{Repository: repo, User: proto.Clone(user).(*gitalypb.User), Branch: []byte("main")},
				},
			},
			getUser: func(m proto.Message) *gitalypb.User {
				return m.(*gitalypb.UserRebaseConfirmableRequest).GetHeader().GetUser()
			},
		},
		"ResolveConflicts": {
			req: &gitalypb.ResolveConflictsRequest{
				ResolveConflictsRequestPayload: &gitalypb.ResolveConflictsRequest_Header{
					Header: &gitalypb.ResolveConflictsRequestHeader{Repository: repo, User: proto.Clone(user).(*gitalypb.User), SourceBranch: []byte("main")},
				},
			},
			getUser: func(m proto.Message) *gitalypb.User {
				return m.(*gitalypb.ResolveConflictsRequest).GetHeader().GetUser()
			},
		},
	}
}

// TestUnaryServerInterceptor_NestedUserIdentity checks that the identity of a
// gitaly.User nested in a header oneof is handled like a top-level one: an
// agent cannot claim another actor through it, and other callers keep their
// values and only get empty fields filled.
func TestUnaryServerInterceptor_NestedUserIdentity(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		desc                 string
		ctx                  func(t *testing.T) context.Context
		user                 *gitalypb.User
		wantID, wantUsername string
	}{
		{
			desc:         "agent cannot claim another user",
			ctx:          func(t *testing.T) context.Context { return agentContext(t, "ci") },
			user:         &gitalypb.User{GlId: "admin", GlUsername: "root", Name: []byte("Ada"), Email: []byte("ada@example.com")},
			wantID:       "ci",
			wantUsername: "ci",
		},
		{
			desc:         "agent with empty identity fields",
			ctx:          func(t *testing.T) context.Context { return agentContext(t, "ci") },
			user:         &gitalypb.User{Name: []byte("Ada"), Email: []byte("ada@example.com")},
			wantID:       "ci",
			wantUsername: "ci",
		},
		{
			desc:         "shared token keeps the client values",
			ctx:          sharedTokenContext,
			user:         &gitalypb.User{GlId: "admin", GlUsername: "root", Name: []byte("Ada"), Email: []byte("ada@example.com")},
			wantID:       "admin",
			wantUsername: "root",
		},
		{
			desc:         "shared token gets empty fields filled",
			ctx:          sharedTokenContext,
			user:         &gitalypb.User{GlUsername: "root", Name: []byte("Ada"), Email: []byte("ada@example.com")},
			wantID:       "shared",
			wantUsername: "root",
		},
		{
			desc:         "anonymous keeps the client values",
			ctx:          func(t *testing.T) context.Context { return testhelper.Context(t) },
			user:         &gitalypb.User{GlId: "admin", GlUsername: "root", Name: []byte("Ada"), Email: []byte("ada@example.com")},
			wantID:       "admin",
			wantUsername: "root",
		},
	}

	for _, tc := range testCases {
		for rpc, rc := range nestedUserRequests(tc.user) {
			t.Run(tc.desc+"/"+rpc, func(t *testing.T) {
				t.Parallel()

				handled, err := runUnary(t, tc.ctx(t), rc.req)
				require.NoError(t, err)

				user := rc.getUser(handled)
				require.Equal(t, tc.wantID, user.GetGlId())
				require.Equal(t, tc.wantUsername, user.GetGlUsername())
				require.Equal(t, "Ada", string(user.GetName()), "non-identity user fields are never changed")
				require.Equal(t, "ada@example.com", string(user.GetEmail()), "non-identity user fields are never changed")
			})
		}
	}
}

// TestStreamServerInterceptor_NestedUserIdentity checks that the header of a
// streaming write RPC sent by an agent carries the agent identity, and that
// the following messages of the stream are left alone.
func TestStreamServerInterceptor_NestedUserIdentity(t *testing.T) {
	t.Parallel()

	for rpc, rc := range nestedUserRequests(&gitalypb.User{GlId: "admin", GlUsername: "root", Name: []byte("Ada")}) {
		t.Run(rpc, func(t *testing.T) {
			t.Parallel()

			stream := &fakeRecvStream{ctx: agentContext(t, "ci"), messages: []proto.Message{rc.req}}

			var got proto.Message
			require.NoError(t, StreamServerInterceptor(nil, stream, &grpc.StreamServerInfo{},
				func(_ any, ss grpc.ServerStream) error {
					got = rc.req.ProtoReflect().Type().New().Interface()
					return ss.RecvMsg(got)
				}))

			user := rc.getUser(got)
			require.Equal(t, "ci", user.GetGlId())
			require.Equal(t, "ci", user.GetGlUsername())
			require.Equal(t, "Ada", string(user.GetName()))
		})
	}

	t.Run("payload messages are left alone", func(t *testing.T) {
		t.Parallel()

		content := &gitalypb.UserCommitFilesRequest{
			UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{
				Action: &gitalypb.UserCommitFilesAction{
					UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Content{Content: []byte("gl_id")},
				},
			},
		}
		stream := &fakeRecvStream{ctx: agentContext(t, "ci"), messages: []proto.Message{content}}

		got := &gitalypb.UserCommitFilesRequest{}
		require.NoError(t, StreamServerInterceptor(nil, stream, &grpc.StreamServerInfo{},
			func(_ any, ss grpc.ServerStream) error { return ss.RecvMsg(got) }))

		testhelper.ProtoEqual(t, content, got)
	})
}

// wrapperMessageType returns a dynamic message type with a repeated
// gitaly.User field "users" and a map<string, gitaly.User> field "by_name", to
// check that identities are found in lists and maps too.
func wrapperMessageType(t *testing.T) protoreflect.MessageType {
	t.Helper()

	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:       proto.String("graft_requestdefaults_test.proto"),
		Package:    proto.String("graft.test"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"shared.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Wrapper"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:     proto.String("users"),
					Number:   proto.Int32(1),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					TypeName: proto.String(".gitaly.User"),
					JsonName: proto.String("users"),
				},
				{
					Name:     proto.String("by_name"),
					Number:   proto.Int32(2),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					TypeName: proto.String(".graft.test.Wrapper.ByNameEntry"),
					JsonName: proto.String("byName"),
				},
			},
			NestedType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("ByNameEntry"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("key"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						JsonName: proto.String("key"),
					},
					{
						Name:     proto.String("value"),
						Number:   proto.Int32(2),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".gitaly.User"),
						JsonName: proto.String("value"),
					},
				},
				Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
			}},
		}},
	}, protoregistry.GlobalFiles)
	require.NoError(t, err)

	return dynamicpb.NewMessageType(file.Messages().ByName("Wrapper"))
}

func TestFillDefaults_ListsAndMaps(t *testing.T) {
	t.Parallel()

	msgType := wrapperMessageType(t)
	md := msgType.Descriptor()
	usersField := md.Fields().ByName("users")
	byNameField := md.Fields().ByName("by_name")

	newUser := func(glID string) protoreflect.Value {
		return protoreflect.ValueOfMessage((&gitalypb.User{GlId: glID, Name: []byte("Ada")}).ProtoReflect())
	}

	msg := msgType.New()
	users := msg.Mutable(usersField).List()
	users.Append(newUser("admin"))
	users.Append(newUser(""))
	byName := msg.Mutable(byNameField).Map()
	byName.Set(protoreflect.ValueOfString("a").MapKey(), newUser("root"))

	fillDefaults(agentContext(t, "ci"), msg.Interface())

	users = msg.Get(usersField).List()
	require.Equal(t, 2, users.Len())
	for i := 0; i < users.Len(); i++ {
		user := users.Get(i).Message().Interface().(*gitalypb.User)
		require.Equal(t, "ci", user.GetGlId())
		require.Equal(t, "ci", user.GetGlUsername())
		require.Equal(t, "Ada", string(user.GetName()))
	}

	mapped := msg.Get(byNameField).Map().Get(protoreflect.ValueOfString("a").MapKey()).Message().Interface().(*gitalypb.User)
	require.Equal(t, "ci", mapped.GetGlId())
	require.Equal(t, "ci", mapped.GetGlUsername())
}

func TestMayHoldIdentity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		msg  proto.Message
		want bool
	}{
		{msg: &gitalypb.User{}, want: true},
		{msg: &gitalypb.UserCreateBranchRequest{}, want: true},
		{msg: &gitalypb.UserCommitFilesRequest{}, want: true},
		{msg: &gitalypb.UserApplyPatchRequest{}, want: true},
		{msg: &gitalypb.UserRebaseConfirmableRequest{}, want: true},
		{msg: &gitalypb.ResolveConflictsRequest{}, want: true},
		{msg: &gitalypb.SSHReceivePackRequest{}, want: true},
		{msg: &gitalypb.FindCommitRequest{}, want: false},
		{msg: &gitalypb.Repository{}, want: false},
		// structpb.Value is recursive through lists and maps.
		{msg: &structpb.Value{}, want: false},
	} {
		md := tc.msg.ProtoReflect().Descriptor()
		// The second call is served from the cache.
		require.Equal(t, tc.want, mayHoldIdentity(md), md.FullName())
		require.Equal(t, tc.want, mayHoldIdentity(md), md.FullName())
	}
}

func TestUnaryServerInterceptor_TopLevelStrings(t *testing.T) {
	t.Parallel()

	// SSHReceivePackRequest carries gl_id and gl_username as top-level strings.
	// An agent's empty gl_id is filled and its claimed gl_username is replaced.
	req := &gitalypb.SSHReceivePackRequest{
		Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"},
		GlId:       "",
		GlUsername: "claimed",
	}

	handled, err := runUnary(t, agentContext(t, "agent-a"), req)
	require.NoError(t, err)

	got := handled.(*gitalypb.SSHReceivePackRequest)
	require.Equal(t, "agent-a", got.GetGlId())
	require.Equal(t, "agent-a", got.GetGlUsername())
	require.Equal(t, "graft:default/repo.git", got.GetRepository().GetGlRepository())
}

// TestUnaryServerInterceptor_CallerIdentity checks that an agent cannot claim
// another actor through the identity fields, and that shared-token and
// anonymous callers keep the values they sent.
func TestUnaryServerInterceptor_CallerIdentity(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		desc string
		ctx  func(t *testing.T) context.Context
		// wantID and wantUsername are the expected values of the User identity
		// fields, and wantGlID and wantGlUsername those of the top-level
		// SSHReceivePackRequest fields.
		wantID, wantUsername, wantGlID, wantGlUsername string
	}{
		{
			desc:           "agent cannot claim another user",
			ctx:            func(t *testing.T) context.Context { return agentContext(t, "ci") },
			wantID:         "ci",
			wantUsername:   "ci",
			wantGlID:       "ci",
			wantGlUsername: "ci",
		},
		{
			desc:           "shared token keeps the client values",
			ctx:            sharedTokenContext,
			wantID:         "admin",
			wantUsername:   "root",
			wantGlID:       "admin",
			wantGlUsername: "root",
		},
		{
			desc:           "anonymous keeps the client values",
			ctx:            func(t *testing.T) context.Context { return testhelper.Context(t) },
			wantID:         "admin",
			wantUsername:   "root",
			wantGlID:       "admin",
			wantGlUsername: "root",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			repo := &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"}

			userReq := &gitalypb.UserCreateBranchRequest{
				Repository: repo,
				User:       &gitalypb.User{GlId: "admin", GlUsername: "root", Name: []byte("Ada")},
			}
			handled, err := runUnary(t, tc.ctx(t), userReq)
			require.NoError(t, err)

			user := handled.(*gitalypb.UserCreateBranchRequest).GetUser()
			require.Equal(t, tc.wantID, user.GetGlId())
			require.Equal(t, tc.wantUsername, user.GetGlUsername())
			require.Equal(t, "Ada", string(user.GetName()), "non-identity user fields are never changed")

			sshReq := &gitalypb.SSHReceivePackRequest{Repository: repo, GlId: "admin", GlUsername: "root"}
			handled, err = runUnary(t, tc.ctx(t), sshReq)
			require.NoError(t, err)

			got := handled.(*gitalypb.SSHReceivePackRequest)
			require.Equal(t, tc.wantGlID, got.GetGlId())
			require.Equal(t, tc.wantGlUsername, got.GetGlUsername())
		})
	}
}

func TestUnaryServerInterceptor_HandlerError(t *testing.T) {
	t.Parallel()

	handlerErr := errors.New("handler failed")
	_, err := UnaryServerInterceptor(agentContext(t, "agent-a"),
		&gitalypb.UserCreateBranchRequest{Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "r"}},
		&grpc.UnaryServerInfo{},
		func(context.Context, any) (any, error) { return nil, handlerErr })

	require.ErrorIs(t, err, handlerErr)
}

func TestUnaryServerInterceptor_NonProtoRequest(t *testing.T) {
	t.Parallel()

	req := "not a proto message"
	var handled any
	_, err := UnaryServerInterceptor(testhelper.Context(t), req, &grpc.UnaryServerInfo{},
		func(_ context.Context, r any) (any, error) {
			handled = r
			return nil, nil
		})

	require.NoError(t, err)
	require.Equal(t, req, handled)
}

func TestUnaryServerInterceptor_Concurrent(t *testing.T) {
	t.Parallel()

	const workers = 32

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			name := fmt.Sprintf("agent-%d", i)
			ctx := identity.NewContext(context.Background(), identity.Identity{Name: name, Source: identity.SourceAgent})
			req := &gitalypb.UserCreateBranchRequest{
				Repository: &gitalypb.Repository{StorageName: "default", RelativePath: fmt.Sprintf("repo-%d.git", i)},
				User:       &gitalypb.User{},
			}

			handled, err := UnaryServerInterceptor(ctx, req, &grpc.UnaryServerInfo{},
				func(_ context.Context, r any) (any, error) { return r, nil })
			if err != nil {
				errs <- err
				return
			}

			got := handled.(*gitalypb.UserCreateBranchRequest)
			if got.GetUser().GetGlId() != name {
				errs <- fmt.Errorf("worker %d: gl_id %q, want %q", i, got.GetUser().GetGlId(), name)
			}
			if want := fmt.Sprintf("graft:default/repo-%d.git", i); got.GetRepository().GetGlRepository() != want {
				errs <- fmt.Errorf("worker %d: gl_repository %q, want %q", i, got.GetRepository().GetGlRepository(), want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
}

// fakeRecvStream yields the queued messages from RecvMsg, then io.EOF.
type fakeRecvStream struct {
	grpc.ServerStream

	ctx      context.Context
	messages []proto.Message
	err      error
}

func (s *fakeRecvStream) Context() context.Context { return s.ctx }

func (s *fakeRecvStream) RecvMsg(m any) error {
	if s.err != nil {
		return s.err
	}
	if len(s.messages) == 0 {
		return io.EOF
	}

	proto.Reset(m.(proto.Message))
	proto.Merge(m.(proto.Message), s.messages[0])
	s.messages = s.messages[1:]
	return nil
}

// TestStreamServerInterceptor_AgentIdentityOverwrite checks that every message
// of a stream from an agent carries the agent identity, even when the client
// claimed another one.
func TestStreamServerInterceptor_AgentIdentityOverwrite(t *testing.T) {
	t.Parallel()

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"}
	stream := &fakeRecvStream{
		ctx: agentContext(t, "ci"),
		messages: []proto.Message{
			&gitalypb.UserCreateBranchRequest{Repository: repo, User: &gitalypb.User{GlId: "admin"}},
			&gitalypb.UserCreateBranchRequest{Repository: repo, User: &gitalypb.User{GlUsername: "root"}},
		},
	}

	var received []*gitalypb.UserCreateBranchRequest
	require.NoError(t, StreamServerInterceptor(nil, stream, &grpc.StreamServerInfo{},
		func(_ any, ss grpc.ServerStream) error {
			for {
				req := &gitalypb.UserCreateBranchRequest{}
				if err := ss.RecvMsg(req); err != nil {
					if errors.Is(err, io.EOF) {
						return nil
					}
					return err
				}
				received = append(received, req)
			}
		}))

	require.Len(t, received, 2)
	require.Equal(t, "ci", received[0].GetUser().GetGlId())
	require.Equal(t, "ci", received[0].GetUser().GetGlUsername())
	require.Equal(t, "ci", received[1].GetUser().GetGlId())
	require.Equal(t, "ci", received[1].GetUser().GetGlUsername())
}

func TestStreamServerInterceptor(t *testing.T) {
	t.Parallel()

	repo := func() *gitalypb.Repository {
		return &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"}
	}

	t.Run("fills defaults of every received message", func(t *testing.T) {
		t.Parallel()

		stream := &fakeRecvStream{
			ctx: agentContext(t, "agent-a"),
			messages: []proto.Message{
				&gitalypb.UserCreateBranchRequest{Repository: repo(), User: &gitalypb.User{}, BranchName: []byte("one")},
				&gitalypb.UserCreateBranchRequest{BranchName: []byte("two")},
			},
		}

		var received []*gitalypb.UserCreateBranchRequest
		err := StreamServerInterceptor(nil, stream, &grpc.StreamServerInfo{},
			func(_ any, ss grpc.ServerStream) error {
				for {
					req := &gitalypb.UserCreateBranchRequest{}
					if err := ss.RecvMsg(req); err != nil {
						if errors.Is(err, io.EOF) {
							return nil
						}
						return err
					}
					received = append(received, req)
				}
			})

		require.NoError(t, err)
		require.Len(t, received, 2)

		// The first message gets its sent user filled and its repository filled.
		require.Equal(t, "agent-a", received[0].GetUser().GetGlId())
		require.Equal(t, "graft:default/repo.git", received[0].GetRepository().GetGlRepository())

		// A user the client did not send is never created, in any message.
		require.Nil(t, received[1].GetUser())
		require.Equal(t, []byte("two"), received[1].GetBranchName())
	})

	t.Run("missing user is not created on any message", func(t *testing.T) {
		t.Parallel()

		stream := &fakeRecvStream{
			ctx: agentContext(t, "agent-a"),
			messages: []proto.Message{
				&gitalypb.UserCreateBranchRequest{Repository: repo(), BranchName: []byte("one")},
			},
		}

		var got *gitalypb.UserCreateBranchRequest
		require.NoError(t, StreamServerInterceptor(nil, stream, &grpc.StreamServerInfo{},
			func(_ any, ss grpc.ServerStream) error {
				got = &gitalypb.UserCreateBranchRequest{}
				return ss.RecvMsg(got)
			}))

		require.Nil(t, got.GetUser())
		require.Equal(t, "graft:default/repo.git", got.GetRepository().GetGlRepository())
	})

	t.Run("present user of a later message is filled", func(t *testing.T) {
		t.Parallel()

		stream := &fakeRecvStream{
			ctx: agentContext(t, "agent-b"),
			messages: []proto.Message{
				&gitalypb.UserCreateBranchRequest{Repository: repo()},
				&gitalypb.UserCreateBranchRequest{User: &gitalypb.User{Name: []byte("Ada")}},
			},
		}

		var received []*gitalypb.UserCreateBranchRequest
		require.NoError(t, StreamServerInterceptor(nil, stream, &grpc.StreamServerInfo{},
			func(_ any, ss grpc.ServerStream) error {
				for {
					req := &gitalypb.UserCreateBranchRequest{}
					if err := ss.RecvMsg(req); err != nil {
						return nil
					}
					received = append(received, req)
				}
			}))

		require.Len(t, received, 2)
		require.Equal(t, "agent-b", received[1].GetUser().GetGlId())
		require.Equal(t, "Ada", string(received[1].GetUser().GetName()))
	})

	t.Run("receive error is passed through untouched", func(t *testing.T) {
		t.Parallel()

		recvErr := errors.New("transport broke")
		stream := &fakeRecvStream{ctx: agentContext(t, "agent-a"), err: recvErr}

		var gotErr error
		require.NoError(t, StreamServerInterceptor(nil, stream, &grpc.StreamServerInfo{},
			func(_ any, ss grpc.ServerStream) error {
				gotErr = ss.RecvMsg(&gitalypb.UserCreateBranchRequest{})
				return nil
			}))

		require.ErrorIs(t, gotErr, recvErr)
	})

	t.Run("handler error is returned", func(t *testing.T) {
		t.Parallel()

		handlerErr := errors.New("handler failed")
		stream := &fakeRecvStream{ctx: agentContext(t, "agent-a")}

		err := StreamServerInterceptor(nil, stream, &grpc.StreamServerInfo{},
			func(_ any, _ grpc.ServerStream) error { return handlerErr })

		require.ErrorIs(t, err, handlerErr)
	})

	t.Run("anonymous without identity", func(t *testing.T) {
		t.Parallel()

		stream := &fakeRecvStream{
			ctx:      testhelper.Context(t),
			messages: []proto.Message{&gitalypb.UserCreateBranchRequest{Repository: repo(), User: &gitalypb.User{}}},
		}

		var got *gitalypb.UserCreateBranchRequest
		require.NoError(t, StreamServerInterceptor(nil, stream, &grpc.StreamServerInfo{},
			func(_ any, ss grpc.ServerStream) error {
				got = &gitalypb.UserCreateBranchRequest{}
				return ss.RecvMsg(got)
			}))

		require.Equal(t, "anonymous", got.GetUser().GetGlId())
	})
}
