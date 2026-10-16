package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	gitalycfgauth "github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	grpcmwauth "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// sharedTokenIdentityName is the identity name of callers authenticated with
// the shared auth.token. Agents may not use this name.
const sharedTokenIdentityName = "shared"

// agentCredential is a resolved [[graft.agent]] credential.
type agentCredential struct {
	name  string
	token string
}

// Interceptors returns the authentication interceptors for a gRPC server.
//
// Agent authentication applies only to the external server and only when at
// least one agent is configured. In every other case, including the internal
// server used by Git hooks, the interceptors behave exactly like
// UnaryServerInterceptor and StreamServerInterceptor.
//
// With agents configured, a request is accepted when it carries one of the
// agent tokens, either as a plain bearer token or as a v2 HMAC token computed
// with the agent token as secret, or when it carries a valid v2 HMAC token
// computed with the shared auth.token (if set). The shared auth.token is
// accepted only as a v2 HMAC token, never as a plain bearer token. The accepted
// caller is stored in the request context as an identity.Identity. A caller
// authenticated with an agent token may only call the RPCs of the allowlist
// (see builtinAgentRPCs and agentAllowlist), plus the [graft] agent_rpcs given
// to InterceptorsWithAgentRPCs. Every other RPC is rejected with
// PermissionDenied, so administrative RPCs stay reserved for the shared token.
// Agents may also not send requests carrying git_config_options (see
// checkAgentRequest) or gitaly-servers metadata (see checkAgentMetadata). All
// of these are rejected with PermissionDenied.
//
// The agent configuration is validated and the token files are read on every
// call, including for the internal server, so an unreadable or invalid agent
// configuration is reported at startup even when no external server is
// created. Health checks stay unauthenticated.
//
// auth.token is optional when agents are configured. Gitaly's own client pool
// only sends credentials when auth.token is set, so Gitaly-to-Gitaly calls
// that dial the external address need auth.token to be set.
func Interceptors(conf gitalycfgauth.Config, agents []config.GraftAgent, external bool) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor, error) {
	return InterceptorsWithAgentRPCs(conf, agents, nil, external)
}

// InterceptorsWithAgentRPCs is Interceptors with the extra RPCs that agents may
// call, the [graft] agent_rpcs, on top of the built-in allowlist. Each entry is
// a full method name of a registered RPC that is not never-allowed (see
// validateAgentRPC); an invalid entry is an error, reported at startup on
// every server, like an invalid agent.
func InterceptorsWithAgentRPCs(conf gitalycfgauth.Config, agents []config.GraftAgent, agentRPCs []string, external bool) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor, error) {
	allowlist, err := newAgentAllowlist(agentRPCs)
	if err != nil {
		return nil, nil, err
	}

	creds, err := loadAgentCredentials(conf.GetToken(), agents)
	if err != nil {
		return nil, nil, err
	}

	// In transitioning mode a request that fails authentication carries no
	// identity and is let through. With agents configured that would also let
	// a request with a wrong agent token skip the allowlist, so the two cannot
	// be combined.
	if conf.Transitioning && len(agents) > 0 {
		return nil, nil, fmt.Errorf("auth.transitioning must be false when [[graft.agent]] is configured: a request with an invalid agent token would skip the allowlist")
	}

	if !external || len(agents) == 0 {
		return UnaryServerInterceptor(conf), StreamServerInterceptor(conf), nil
	}

	check := agentCheckFunc(conf, creds)
	authUnary, authStream := withAgentAuthorization(allowlist, grpcmwauth.UnaryServerInterceptor(check), grpcmwauth.StreamServerInterceptor(check))

	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return authUnary(ctx, req, info, func(ctx context.Context, req any) (any, error) {
			if err := checkAgentMetadata(ctx); err != nil {
				return nil, err
			}
			if err := checkAgentRequest(ctx, req); err != nil {
				return nil, err
			}
			return handler(ctx, req)
		})
	}

	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return authStream(srv, ss, info, func(srv any, ss grpc.ServerStream) error {
			if err := checkAgentMetadata(ss.Context()); err != nil {
				return err
			}
			return handler(srv, &agentRequestGuardStream{ServerStream: ss})
		})
	}

	return unary, stream, nil
}

// checkAgentMetadata rejects a request of an agent that carries metadata agents
// may not send. Other identities are not restricted.
//
// Agents may not send gitaly-servers metadata. It maps storage names to the
// address and token of a Gitaly server, and RPCs that work across repositories,
// such as UserCommitFiles with a start_repository on another storage, dial the
// address it names and fetch from it. An agent could therefore make the server
// connect to any address it likes, including internal network addresses and
// Unix sockets (server-side request forgery). Without the metadata, such a
// request fails before anything is dialed.
func checkAgentMetadata(ctx context.Context) error {
	id, ok := identity.FromContext(ctx)
	if !ok || id.Source != identity.SourceAgent {
		return nil
	}

	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get(storage.GitalyServersMetadataKey)) > 0 {
		return status.Errorf(codes.PermissionDenied, "agent %q may not send %s metadata", id.Name, storage.GitalyServersMetadataKey)
	}

	return nil
}

// gitConfigOptionsFieldName is the name of the request field whose values are
// passed to Git as `-c key=value` options (SSH and smart HTTP RPCs).
const gitConfigOptionsFieldName = protoreflect.Name("git_config_options")

// checkAgentRequest rejects a request message of an agent that carries
// options agents may not set. Other identities are not restricted.
//
// Agents may not send git_config_options: Git applies these options after
// the ones Gitaly sets, so they could for example override core.hooksPath to
// skip the access checks and the reference-transaction hook of a push, or set
// uploadpack.packObjectsHook to run an arbitrary command.
func checkAgentRequest(ctx context.Context, req any) error {
	id, ok := identity.FromContext(ctx)
	if !ok || id.Source != identity.SourceAgent {
		return nil
	}

	msg, ok := req.(proto.Message)
	if !ok {
		return nil
	}

	m := msg.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(gitConfigOptionsFieldName)
	if fd != nil && fd.IsList() && m.Get(fd).List().Len() > 0 {
		return status.Errorf(codes.PermissionDenied, "agent %q may not set %s", id.Name, gitConfigOptionsFieldName)
	}

	return nil
}

// agentRequestGuardStream checks every message an agent sends on a stream.
type agentRequestGuardStream struct {
	grpc.ServerStream
}

// RecvMsg receives the next message and rejects it when the agent may not
// send it.
func (s *agentRequestGuardStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}

	return checkAgentRequest(s.Context(), m)
}

// loadAgentCredentials validates the agent configuration and resolves every
// agent token, reading token files from disk. sharedToken is the auth.token
// and may be empty.
func loadAgentCredentials(sharedToken string, agents []config.GraftAgent) ([]agentCredential, error) {
	creds := make([]agentCredential, 0, len(agents))
	names := make(map[string]struct{}, len(agents))
	tokens := make(map[string]struct{}, len(agents))

	for i, agent := range agents {
		if agent.Name == "" {
			return nil, fmt.Errorf("graft agent %d: name must not be empty", i)
		}
		if agent.Name == sharedTokenIdentityName {
			return nil, fmt.Errorf("graft agent %d: name %q is reserved", i, agent.Name)
		}
		if _, ok := names[agent.Name]; ok {
			return nil, fmt.Errorf("graft agent %q: duplicate name", agent.Name)
		}
		names[agent.Name] = struct{}{}

		token, err := resolveAgentToken(agent)
		if err != nil {
			return nil, fmt.Errorf("graft agent %q: %w", agent.Name, err)
		}
		// The shared token is accepted only as a v2 HMAC secret. An agent with the
		// same token would be accepted as a plain bearer token and attributed to
		// the agent instead of the shared caller.
		if sharedToken != "" && token == sharedToken {
			return nil, fmt.Errorf("graft agent %q: token is the same as auth.token", agent.Name)
		}
		if _, ok := tokens[token]; ok {
			return nil, fmt.Errorf("graft agent %q: token is already used by another agent", agent.Name)
		}
		tokens[token] = struct{}{}

		creds = append(creds, agentCredential{name: agent.Name, token: token})
	}

	return creds, nil
}

func resolveAgentToken(agent config.GraftAgent) (string, error) {
	token, err := readAgentToken(agent)
	if err != nil {
		return "", err
	}

	// A token is sent as a bearer token in the authorization metadata, so it
	// must be a single line without whitespace or control characters.
	if strings.IndexFunc(token, isUnsafeTokenRune) >= 0 {
		return "", fmt.Errorf("token must not contain whitespace or control characters")
	}

	return token, nil
}

func readAgentToken(agent config.GraftAgent) (string, error) {
	switch {
	case agent.Token != "" && agent.TokenFile != "":
		return "", fmt.Errorf("token and token_file are mutually exclusive")
	case agent.TokenFile != "":
		data, err := os.ReadFile(agent.TokenFile)
		if err != nil {
			return "", fmt.Errorf("reading token file: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("token file %q is empty", agent.TokenFile)
		}
		return token, nil
	case agent.Token != "":
		return agent.Token, nil
	default:
		return "", fmt.Errorf("either token or token_file must be set")
	}
}

// isUnsafeTokenRune reports whether r cannot appear in a token.
func isUnsafeTokenRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r)
}

// agentCheckFunc returns the authentication function used with agents
// configured. It mirrors checkFunc for metrics and transitioning mode.
func agentCheckFunc(conf gitalycfgauth.Config, agents []agentCredential) grpcmwauth.AuthFunc {
	return func(ctx context.Context) (context.Context, error) {
		id, err := authenticateAgent(ctx, conf, agents, time.Now())
		switch status.Code(err) {
		case codes.OK:
			countStatus(okLabel(conf.Transitioning), conf.Transitioning).Inc()
		default:
			countStatus("unauthenticated", conf.Transitioning).Inc()
		}

		if err != nil {
			if conf.Transitioning {
				return ctx, nil
			}
			return ctx, err
		}

		return identity.NewContext(ctx, id), nil
	}
}

// authenticateAgent returns the identity of the caller carried by ctx.
func authenticateAgent(ctx context.Context, conf gitalycfgauth.Config, agents []agentCredential, now time.Time) (identity.Identity, error) {
	if bearer, err := grpcmwauth.AuthFromMD(ctx, "bearer"); err == nil {
		for _, agent := range agents {
			if subtle.ConstantTimeCompare([]byte(bearer), []byte(agent.token)) == 1 {
				return identity.Identity{Name: agent.name, Source: identity.SourceAgent}, nil
			}
		}
	}

	for _, agent := range agents {
		if gitalyauth.CheckToken(ctx, agent.token, now) == nil {
			return identity.Identity{Name: agent.name, Source: identity.SourceAgent}, nil
		}
	}

	if shared := conf.GetToken(); shared != "" && gitalyauth.CheckToken(ctx, shared, now) == nil {
		return identity.Identity{Name: sharedTokenIdentityName, Source: identity.SourceSharedToken}, nil
	}

	return identity.Identity{}, status.Error(codes.Unauthenticated, "authentication required")
}
