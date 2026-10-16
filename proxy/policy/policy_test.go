package policy

import (
	"errors"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/proxy/policy/testdata"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestCheckUniversalInvariants(t *testing.T) {
	for _, test := range []struct {
		name, method, reason string
		code                 codes.Code
	}{
		{name: "known", method: "/gitaly.BlobService/ListBlobs", code: codes.OK},
		{name: "unknown", method: "/gitaly.BlobService/Unknown", code: codes.PermissionDenied, reason: "proxy: unknown_method"},
		{name: "excluded", method: "/gitaly.SmartHTTPService/InfoRefsUploadPack", code: codes.PermissionDenied, reason: "proxy: method_not_allowed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := CheckUniversalInvariants(test.method)
			require.Equal(t, test.code, status.Code(err))
			if test.reason != "" {
				require.Equal(t, test.reason, status.Convert(err).Message())
			}
		})
	}
}

func TestReadonlyRepositoryMethodInvariants(t *testing.T) {
	profile, ok := LookupProfile(ProfileReadonlyRepository)
	require.True(t, ok)

	for _, test := range []struct{ name, method, reason string }{
		{name: "valid unary", method: "/gitaly.RepositoryService/RepositoryExists"},
		{name: "valid server streaming", method: "/gitaly.BlobService/ListBlobs"},
		{name: "mutator", method: "/gitaly.CommitService/CommitLanguages", reason: "proxy: not_accessor"},
		{name: "not repository scoped", method: "/gitaly.PartitionService/ListPartitions", reason: "proxy: not_repository_scoped"},
		{name: "client streaming", method: "/gitaly.CommitService/CheckObjectsExist", reason: "proxy: client_streaming_not_allowed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			method, err := CheckUniversalInvariants(test.method)
			require.NoError(t, err)
			err = profile.CheckMethod(method)
			if test.reason == "" {
				require.NoError(t, err)
				return
			}
			require.Equal(t, test.reason, status.Convert(err).Message())
		})
	}
}

func methodWithInput(t *testing.T, inputType string) Method {
	t.Helper()
	file := protodesc.ToFileDescriptorProto(testdata.File_proxy_policy_testdata_ambiguous_proto)
	options := &descriptorpb.MethodOptions{}
	proto.SetExtension(options, gitalypb.E_OpType, &gitalypb.OperationMsg{
		Op:         gitalypb.OperationMsg_ACCESSOR,
		ScopeLevel: gitalypb.OperationMsg_REPOSITORY,
	})
	file.Service = append(file.Service, &descriptorpb.ServiceDescriptorProto{
		Name: proto.String("TestService"),
		Method: []*descriptorpb.MethodDescriptorProto{{
			Name:       proto.String("TestMethod"),
			InputType:  proto.String(inputType),
			OutputType: proto.String(inputType),
			Options:    options,
		}},
	})
	registry, err := protoregistry.New(file)
	require.NoError(t, err)
	info, err := registry.LookupMethod("/gitalyproxytest.TestService/TestMethod")
	require.NoError(t, err)
	return Method{info: info}
}

func ambiguousMethod(t *testing.T) Method {
	t.Helper()
	return methodWithInput(t, ".gitalyproxytest.AmbiguousRequest")
}

func TestReadonlyRepositoryAnnotationInvariants(t *testing.T) {
	profile, ok := LookupProfile(ProfileReadonlyRepository)
	require.True(t, ok)

	for _, test := range []struct {
		name, inputType, reason string
	}{
		{name: "additional repository", inputType: ".gitaly.FetchIntoObjectPoolRequest", reason: "proxy: additional_repository"},
		{name: "no target repository", inputType: ".gitaly.WalkReposRequest", reason: "proxy: no_target_repository"},
		{name: "ambiguous target repository", inputType: ".gitalyproxytest.AmbiguousRequest", reason: "proxy: ambiguous_target_repository"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := profile.CheckMethod(methodWithInput(t, test.inputType))
			require.Equal(t, test.reason, status.Convert(err).Message())
		})
	}
}

func TestReadonlyRepositoryRejectsAmbiguousRequest(t *testing.T) {
	method := ambiguousMethod(t)
	request := &testdata.AmbiguousRequest{
		Repository: &gitalypb.Repository{StorageName: "first"},
		Nested: &testdata.NestedTarget{
			Repository: &gitalypb.Repository{StorageName: "second"},
		},
	}
	frame, err := proto.Marshal(request)
	require.NoError(t, err)

	profile, ok := LookupProfile(ProfileReadonlyRepository)
	require.True(t, ok)
	err = profile.CheckRequest(method, frame, request.GetRepository())
	require.Equal(t, "proxy: ambiguous_target_repository", status.Convert(err).Message())
}

func TestReadonlyRepositoryRequestInvariants(t *testing.T) {
	method, err := CheckUniversalInvariants("/gitaly.BlobService/ListBlobs")
	require.NoError(t, err)
	profile, ok := LookupProfile(ProfileReadonlyRepository)
	require.True(t, ok)
	expected := &gitalypb.Repository{StorageName: "default", RelativePath: "project.git"}

	for _, test := range []struct {
		name       string
		repository *gitalypb.Repository
		reason     string
	}{
		{name: "matches", repository: expected},
		{name: "storage mismatch", repository: &gitalypb.Repository{StorageName: "other", RelativePath: "project.git"}, reason: "proxy: repo_scope"},
		{name: "path mismatch", repository: &gitalypb.Repository{StorageName: "default", RelativePath: "other.git"}, reason: "proxy: repo_scope"},
		{name: "object directory", repository: &gitalypb.Repository{StorageName: "default", RelativePath: "project.git", GitObjectDirectory: "objects"}, reason: "proxy: object_directory"},
		{name: "alternate directories", repository: &gitalypb.Repository{StorageName: "default", RelativePath: "project.git", GitAlternateObjectDirectories: []string{"alternate"}}, reason: "proxy: object_directory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame, err := proto.Marshal(&gitalypb.ListBlobsRequest{Repository: test.repository})
			require.NoError(t, err)
			err = profile.CheckRequest(method, frame, expected)
			if test.reason == "" {
				require.NoError(t, err)
				return
			}
			require.Equal(t, test.reason, status.Convert(err).Message())
		})
	}
}

func TestReadonlyRepositoryDuplicateWireFieldUsesLastValue(t *testing.T) {
	first, err := proto.Marshal(&gitalypb.Repository{StorageName: "first", RelativePath: "project.git"})
	require.NoError(t, err)
	second, err := proto.Marshal(&gitalypb.Repository{StorageName: "second", RelativePath: "project.git"})
	require.NoError(t, err)

	frame := protowire.AppendTag(nil, 1, protowire.BytesType)
	frame = protowire.AppendBytes(frame, first)
	frame = protowire.AppendTag(frame, 1, protowire.BytesType)
	frame = protowire.AppendBytes(frame, second)

	method, err := CheckUniversalInvariants("/gitaly.BlobService/ListBlobs")
	require.NoError(t, err)
	profile, ok := LookupProfile(ProfileReadonlyRepository)
	require.True(t, ok)
	require.NoError(t, profile.CheckRequest(method, frame, &gitalypb.Repository{
		StorageName:  "second",
		RelativePath: "project.git",
	}))
}

func TestOrbitAllowlistSatisfiesReadonlyRepositoryProfile(t *testing.T) {
	allowlist := []string{
		"/gitaly.RepositoryService/RepositoryExists", "/gitaly.RepositoryService/GetArchive",
		"/gitaly.BlobService/ListBlobs", "/gitaly.BlobService/GetBlob", "/gitaly.BlobService/GetBlobs",
		"/gitaly.BlobService/ListLFSPointers", "/gitaly.CommitService/FindCommit",
		"/gitaly.CommitService/FindCommits", "/gitaly.CommitService/ListCommits",
		"/gitaly.CommitService/GetTreeEntries", "/gitaly.CommitService/TreeEntry",
		"/gitaly.CommitService/ListFiles", "/gitaly.CommitService/LastCommitForPath",
		"/gitaly.CommitService/CommitIsAncestor", "/gitaly.CommitService/ListLastCommitsForTree",
		"/gitaly.CommitService/RawBlame", "/gitaly.DiffService/FindChangedPaths",
		"/gitaly.DiffService/RawDiff", "/gitaly.DiffService/CommitDiff",
		"/gitaly.RefService/FindDefaultBranchName", "/gitaly.RefService/FindBranch",
		"/gitaly.RefService/FindLocalBranches", "/gitaly.RefService/FindAllTags",
		"/gitaly.RefService/FindTag", "/gitaly.RefService/ListRefs",
	}
	profile, ok := LookupProfile(ProfileReadonlyRepository)
	require.True(t, ok)
	methods := NewAllowlist(allowlist)

	for _, fullMethod := range allowlist {
		t.Run(strings.TrimPrefix(fullMethod, "/gitaly."), func(t *testing.T) {
			method, err := CheckUniversalInvariants(fullMethod)
			require.NoError(t, err)
			require.NoError(t, profile.CheckMethod(method))
			require.Equal(t, 1, method.info.TargetRepoFieldCount())
			require.True(t, methods.Allows(fullMethod))
		})
	}

	method, err := CheckUniversalInvariants("/gitaly.CommitService/CommitLanguages")
	require.NoError(t, err)
	err = profile.CheckMethod(method)
	require.Equal(t, "proxy: not_accessor", status.Convert(err).Message())
	var invariant *InvariantError
	require.True(t, errors.As(err, &invariant))
	require.Equal(t, "not_accessor", invariant.Reason())
}
