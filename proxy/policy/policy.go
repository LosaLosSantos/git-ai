// Package policy provides authorization profiles for transparent Gitaly proxy streams.
package policy

import (
	"errors"
	"fmt"

	"github.com/feder-cr/open_scion/internal/grpc/protoregistry"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// ProfileReadonlyRepository identifies the profile that permits read-only RPCs
// against exactly one repository.
const ProfileReadonlyRepository = "readonly_repository"

// excludedServices is the universal denylist from the Workhorse v1 policy.
// These administrative and direct-transport services stay denied regardless
// of individual RPC operation annotations.
var excludedServices = map[string]struct{}{
	"gitaly.OperationService":    {},
	"gitaly.SmartHTTPService":    {},
	"gitaly.SSHService":          {},
	"gitaly.ServerService":       {},
	"gitaly.InternalGitaly":      {},
	"gitaly.ObjectPoolService":   {},
	"gitaly.PraefectInfoService": {},
}

// Method is a Gitaly method that passed the universal proxy checks.
type Method struct {
	info protoregistry.MethodInfo
}

// FullName returns the canonical full gRPC method name.
func (m Method) FullName() string { return m.info.FullMethodName() }

// Profile is a set of compiled authorization invariants.
type Profile interface {
	// Name returns the exact profile name accepted in a grant.
	Name() string
	// CheckMethod applies descriptor-level invariants.
	CheckMethod(Method) error
	// CheckRequest applies first-request-frame invariants.
	CheckRequest(Method, []byte, *gitalypb.Repository) error
}

type readonlyRepository struct{}

func (readonlyRepository) Name() string { return ProfileReadonlyRepository }

func (readonlyRepository) CheckMethod(method Method) error {
	info := method.info
	if info.Operation != protoregistry.OpAccessor {
		return invariantError("not_accessor", nil)
	}
	if info.Scope != protoregistry.ScopeRepository {
		return invariantError("not_repository_scoped", nil)
	}
	if info.IsClientStreaming() {
		return invariantError("client_streaming_not_allowed", nil)
	}
	if info.AdditionalRepoFieldCount() > 0 {
		return invariantError("additional_repository", nil)
	}

	switch info.TargetRepoFieldCount() {
	case 0:
		return invariantError("no_target_repository", ErrNoTargetRepository)
	case 1:
		return nil
	default:
		return invariantError("ambiguous_target_repository", ErrAmbiguousTargetRepository)
	}
}

func (readonlyRepository) CheckRequest(method Method, frame []byte, expected *gitalypb.Repository) error {
	// Validating only the first frame is safe because CheckMethod rejects every
	// client-streaming RPC, so this is the stream's only request message.
	request, err := method.info.UnmarshalRequestProto(frame)
	if err != nil {
		return fmt.Errorf("%w: unmarshal request: %w", ErrMalformedRequest, err)
	}

	repository, err := method.info.TargetRepo(request)
	if err != nil {
		switch {
		case errors.Is(err, protoregistry.ErrRepositoryFieldAmbiguous):
			return invariantError("ambiguous_target_repository", ErrAmbiguousTargetRepository)
		case errors.Is(err, protoregistry.ErrRepositoryFieldNotFound):
			return invariantError("no_target_repository", ErrNoTargetRepository)
		default:
			return fmt.Errorf("target repository: %w", err)
		}
	}

	if expected == nil || repository.GetStorageName() != expected.GetStorageName() || repository.GetRelativePath() != expected.GetRelativePath() {
		return invariantError("repo_scope", nil)
	}
	if repository.GetGitObjectDirectory() != "" || len(repository.GetGitAlternateObjectDirectories()) != 0 {
		return invariantError("object_directory", nil)
	}

	return nil
}

var profiles = map[string]Profile{
	ProfileReadonlyRepository: readonlyRepository{},
}

// LookupProfile returns the compiled profile matching name exactly.
func LookupProfile(name string) (Profile, bool) {
	profile, ok := profiles[name]
	return profile, ok
}

// CheckUniversalInvariants resolves a known Gitaly method and rejects methods
// intercepted by Praefect or belonging to administrative services.
func CheckUniversalInvariants(fullMethod string) (Method, error) {
	registry := protoregistry.GitalyProtoPreregistered
	if registry.IsInterceptedMethod(fullMethod) {
		return Method{}, invariantError("method_not_allowed", nil)
	}

	info, err := registry.LookupMethod(fullMethod)
	if err != nil {
		return Method{}, invariantError("unknown_method", err)
	}
	if _, excluded := excludedServices[info.Package()+"."+info.Service()]; excluded {
		return Method{}, invariantError("method_not_allowed", nil)
	}

	return Method{info: info}, nil
}

// Allowlist narrows a compiled profile to exact full method names.
type Allowlist map[string]struct{}

// NewAllowlist constructs an exact-match allowlist from full method names.
func NewAllowlist(methods []string) Allowlist {
	allowlist := make(Allowlist, len(methods))
	for _, method := range methods {
		allowlist[method] = struct{}{}
	}
	return allowlist
}

// Allows returns whether the exact full method name is present.
func (a Allowlist) Allows(fullMethod string) bool {
	_, ok := a[fullMethod]
	return ok
}
