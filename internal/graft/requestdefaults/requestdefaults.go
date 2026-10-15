// Package requestdefaults fills in request fields that a standalone Graft
// server leaves empty, so that callers do not need to send GitLab-specific
// fields. The values come from the authenticated identity (see
// internal/graft/identity) and from the target repository.
//
// Identity fields are handled wherever they appear in a request: in every
// gitaly.User message reachable from the request (including users nested in
// header messages and oneofs, such as UserCommitFilesRequest.header.user) and
// in every string field named gl_id or gl_username. They are filled when empty.
// The identity fields of a caller authenticated with an agent token
// (SourceAgent) are always set to the agent name, even when the client sent
// another value: the standalone access policy, the Git hooks and the changeset
// log read these fields, so an agent must not be able to claim another actor's
// identity.
//
// The gl_repository of the top-level repository field is filled when empty,
// and no other value is ever overwritten. Absent messages are never created.
// The interceptors are meant to be installed after the authentication
// interceptor.
package requestdefaults

import (
	"context"
	"sync"

	"github.com/feder-cr/open_scion/internal/graft/identity"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	// anonymousName is used when a request carries no identity.
	anonymousName = "anonymous"
	// repositoryPrefix prefixes the gl_repository value of standalone repositories.
	repositoryPrefix = "graft:"

	userMessageName       = protoreflect.FullName("gitaly.User")
	repositoryMessageName = protoreflect.FullName("gitaly.Repository")
	repositoryFieldName   = protoreflect.Name("repository")

	glIDFieldName         = protoreflect.Name("gl_id")
	glUsernameFieldName   = protoreflect.Name("gl_username")
	glRepositoryFieldName = protoreflect.Name("gl_repository")
	storageNameFieldName  = protoreflect.Name("storage_name")
	relativePathFieldName = protoreflect.Name("relative_path")
)

// UnaryServerInterceptor fills in the defaults of a unary request.
func UnaryServerInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if msg, ok := req.(proto.Message); ok {
		fillDefaults(ctx, msg)
	}

	return handler(ctx, req)
}

// StreamServerInterceptor fills in the defaults of every message received on a
// streaming request.
func StreamServerInterceptor(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return handler(srv, &recvDefaultsStream{ServerStream: stream})
}

// recvDefaultsStream fills in the defaults of each received message.
type recvDefaultsStream struct {
	grpc.ServerStream
}

// RecvMsg receives the next message and fills in its defaults.
func (s *recvDefaultsStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}

	if msg, ok := m.(proto.Message); ok {
		fillDefaults(s.Context(), msg)
	}

	return nil
}

// caller describes the identity fields a request is filled with.
type caller struct {
	// name is the value of the identity fields.
	name string
	// overwrite tells whether the identity fields are set even when the client
	// sent a non-empty value. It is true only for agent identities, whose
	// requests must not be able to claim another actor. Shared-token and
	// anonymous callers keep the client's values.
	overwrite bool
}

// callerFromContext returns the identity fields to fill in for ctx.
func callerFromContext(ctx context.Context) caller {
	id, ok := identity.FromContext(ctx)
	if !ok {
		return caller{name: anonymousName}
	}

	return caller{name: id.Name, overwrite: id.Source == identity.SourceAgent}
}

// fillDefaults fills the fields of msg:
//
//   - every gitaly.User message reachable from msg, at any depth, gets its
//     gl_id and gl_username set to the identity name. A User that is unset
//     stays unset: creating one would produce commits and tags without a name
//     or an email;
//   - every string field named gl_id or gl_username of msg or of a message
//     reachable from it gets the identity name;
//   - the top-level gitaly.Repository field named "repository" gets an empty
//     gl_repository of "graft:<storage>/<relative path>".
//
// The identity fields are only set when empty, except for agent callers, for
// which they are always set to the agent name.
func fillDefaults(ctx context.Context, msg proto.Message) {
	m := msg.ProtoReflect()
	if !m.IsValid() {
		return
	}

	fillIdentities(m, callerFromContext(ctx))

	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Name() == repositoryFieldName && fd.Kind() == protoreflect.MessageKind &&
			!fd.IsList() && !fd.IsMap() && fd.Message().FullName() == repositoryMessageName {
			fillRepository(m, fd)
		}
	}
}

// fillIdentities fills the identity fields of m and of every message reachable
// from it. Only populated message fields are descended into, so no message is
// ever created. Message types that cannot contain an identity field are
// skipped.
func fillIdentities(m protoreflect.Message, c caller) {
	if !mayHoldIdentity(m.Descriptor()) {
		return
	}

	// Identity strings are set before ranging over m, because m must not be
	// modified while it is ranged over. Proto3 strings that are empty are not
	// populated, so Range would not visit them.
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		if fd := fields.Get(i); isIdentityField(fd) {
			fillString(m, fd, c)
		}
	}

	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if isMessageKind(fd.MapValue().Kind()) {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					fillIdentities(mv.Message(), c)
					return true
				})
			}
		case !isMessageKind(fd.Kind()):
		case fd.IsList():
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				fillIdentities(list.Get(i).Message(), c)
			}
		default:
			fillIdentities(v.Message(), c)
		}
		return true
	})
}

// isIdentityField reports whether fd is a singular string field named gl_id
// or gl_username.
func isIdentityField(fd protoreflect.FieldDescriptor) bool {
	return fd.Kind() == protoreflect.StringKind && !fd.IsList() && !fd.IsMap() &&
		(fd.Name() == glIDFieldName || fd.Name() == glUsernameFieldName)
}

func isMessageKind(k protoreflect.Kind) bool {
	return k == protoreflect.MessageKind || k == protoreflect.GroupKind
}

// identityHolders caches, per message full name, whether a message of that type
// can contain an identity field, directly or through nested messages.
var identityHolders sync.Map

// mayHoldIdentity reports whether a message of type md can contain a gitaly.User
// or an identity string field at any depth.
func mayHoldIdentity(md protoreflect.MessageDescriptor) bool {
	if cached, ok := identityHolders.Load(md.FullName()); ok {
		return cached.(bool)
	}

	// The result is computed with a traversal of its own, so that the result
	// for a type is never derived from a partial result of a recursive type.
	result := reachesIdentity(md, map[protoreflect.FullName]struct{}{})
	identityHolders.Store(md.FullName(), result)
	return result
}

// reachesIdentity walks the message types reachable from md.
func reachesIdentity(md protoreflect.MessageDescriptor, visited map[protoreflect.FullName]struct{}) bool {
	if _, ok := visited[md.FullName()]; ok {
		return false
	}
	visited[md.FullName()] = struct{}{}

	if md.FullName() == userMessageName {
		return true
	}

	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if isIdentityField(fd) {
			return true
		}

		var nested protoreflect.MessageDescriptor
		switch {
		case fd.IsMap():
			if isMessageKind(fd.MapValue().Kind()) {
				nested = fd.MapValue().Message()
			}
		case isMessageKind(fd.Kind()):
			nested = fd.Message()
		}

		if nested != nil && reachesIdentity(nested, visited) {
			return true
		}
	}

	return false
}

// fillRepository sets the gl_repository of the repository in field fd when it
// is empty and the repository has a storage and a relative path.
func fillRepository(m protoreflect.Message, fd protoreflect.FieldDescriptor) {
	if !m.Has(fd) {
		return
	}

	repo := m.Get(fd).Message()
	fields := repo.Descriptor().Fields()
	glRepository := fields.ByName(glRepositoryFieldName)
	storageName := fields.ByName(storageNameFieldName)
	relativePath := fields.ByName(relativePathFieldName)
	if glRepository == nil || storageName == nil || relativePath == nil {
		return
	}

	if repo.Get(glRepository).String() != "" {
		return
	}

	storage := repo.Get(storageName).String()
	path := repo.Get(relativePath).String()
	if storage == "" || path == "" {
		return
	}

	repo.Set(glRepository, protoreflect.ValueOfString(repositoryPrefix+storage+"/"+path))
}

// fillString sets the string field fd of m to the identity name when it is
// empty, or always for an agent caller.
func fillString(m protoreflect.Message, fd protoreflect.FieldDescriptor, c caller) {
	if !c.overwrite && m.Get(fd).String() != "" {
		return
	}

	m.Set(fd, protoreflect.ValueOfString(c.name))
}
