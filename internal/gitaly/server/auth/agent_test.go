package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	gitalycfgauth "github.com/feder-cr/open_scion/internal/gitaly/config/auth"
	"github.com/feder-cr/open_scion/internal/graft/identity"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	agentAName       = "agent-a"
	agentASecret     = "agent-a-secret"
	agentBName       = "agent-b"
	agentBSecret     = "agent-b-secret"
	sharedSecretTest = "shared-secret"
	testMethod       = "/gitaly.RepositoryService/RepositoryExists"
)

func testAgents() []config.GraftAgent {
	return []config.GraftAgent{
		{Name: agentAName, Token: agentASecret},
		{Name: agentBName, Token: agentBSecret},
	}
}

// v2Authorization returns a v2 HMAC authorization header value signed with
// secret for the given issue time.
func v2Authorization(secret string, issuedAt time.Time) string {
	message := strconv.FormatInt(issuedAt.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(message))

	return "Bearer v2." + hex.EncodeToString(mac.Sum(nil)) + "." + message
}

// callUnary runs the unary interceptor with a handler that reports the identity
// found in the context of the handler.
func callUnary(t *testing.T, ctx context.Context, interceptor grpc.UnaryServerInterceptor, srv any) (identity.Identity, bool, error) {
	t.Helper()

	var got identity.Identity
	var found bool
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: testMethod, Server: srv},
		func(ctx context.Context, _ any) (any, error) {
			got, found = identity.FromContext(ctx)
			return nil, nil
		})

	return got, found, err
}

func contextWithAuthorization(t *testing.T, authorization string) context.Context {
	t.Helper()

	ctx := testhelper.Context(t)
	if authorization == "" {
		return ctx
	}

	return metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", authorization))
}

func TestInterceptors_AgentTokens(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sharedConf := gitalycfgauth.Config{Token: sharedSecretTest}

	testCases := []struct {
		desc          string
		conf          gitalycfgauth.Config
		agents        []config.GraftAgent
		external      bool
		server        any
		authorization string
		wantCode      codes.Code
		wantIdentity  bool
		wantID        identity.Identity
	}{
		{
			desc:          "agent plain bearer token",
			conf:          sharedConf,
			agents:        testAgents(),
			external:      true,
			authorization: "Bearer " + agentASecret,
			wantCode:      codes.OK,
			wantIdentity:  true,
			wantID:        identity.Identity{Name: agentAName, Source: identity.SourceAgent},
		},
		{
			desc:          "second agent plain bearer token",
			conf:          sharedConf,
			agents:        testAgents(),
			external:      true,
			authorization: "Bearer " + agentBSecret,
			wantCode:      codes.OK,
			wantIdentity:  true,
			wantID:        identity.Identity{Name: agentBName, Source: identity.SourceAgent},
		},
		{
			desc:          "agent v2 HMAC token signed with agent token",
			conf:          gitalycfgauth.Config{},
			agents:        testAgents(),
			external:      true,
			authorization: v2Authorization(agentBSecret, now),
			wantCode:      codes.OK,
			wantIdentity:  true,
			wantID:        identity.Identity{Name: agentBName, Source: identity.SourceAgent},
		},
		{
			desc:          "shared token v2 HMAC is accepted with agents configured",
			conf:          sharedConf,
			agents:        testAgents(),
			external:      true,
			authorization: v2Authorization(sharedSecretTest, now),
			wantCode:      codes.OK,
			wantIdentity:  true,
			wantID:        identity.Identity{Name: sharedTokenIdentityName, Source: identity.SourceSharedToken},
		},
		{
			desc:          "shared token is not accepted when auth.token is unset",
			conf:          gitalycfgauth.Config{},
			agents:        testAgents(),
			external:      true,
			authorization: v2Authorization(sharedSecretTest, now),
			wantCode:      codes.Unauthenticated,
		},
		{
			// The shared auth.token is accepted only as a v2 HMAC token. The plain
			// bearer form is rejected, even though the shared token is configured.
			desc:          "shared token as plain bearer token is rejected",
			conf:          sharedConf,
			agents:        testAgents(),
			external:      true,
			authorization: "Bearer " + sharedSecretTest,
			wantCode:      codes.Unauthenticated,
		},
		{
			desc:          "wrong plain token",
			conf:          sharedConf,
			agents:        testAgents(),
			external:      true,
			authorization: "Bearer not-a-token",
			wantCode:      codes.Unauthenticated,
		},
		{
			desc:          "plain token that is a prefix of an agent token",
			conf:          sharedConf,
			agents:        testAgents(),
			external:      true,
			authorization: "Bearer " + agentASecret[:5],
			wantCode:      codes.Unauthenticated,
		},
		{
			desc:          "v2 token signed with an unknown secret",
			conf:          sharedConf,
			agents:        testAgents(),
			external:      true,
			authorization: v2Authorization("unknown-secret", now),
			wantCode:      codes.Unauthenticated,
		},
		{
			desc:          "expired v2 token signed with agent token",
			conf:          sharedConf,
			agents:        testAgents(),
			external:      true,
			authorization: v2Authorization(agentASecret, now.Add(-time.Hour)),
			wantCode:      codes.Unauthenticated,
		},
		{
			desc:          "future v2 token signed with agent token",
			conf:          sharedConf,
			agents:        testAgents(),
			external:      true,
			authorization: v2Authorization(agentASecret, now.Add(time.Hour)),
			wantCode:      codes.Unauthenticated,
		},
		{
			desc:          "non bearer scheme",
			conf:          sharedConf,
			agents:        testAgents(),
			external:      true,
			authorization: "Basic " + agentASecret,
			wantCode:      codes.Unauthenticated,
		},
		{
			desc:     "no credentials",
			conf:     sharedConf,
			agents:   testAgents(),
			external: true,
			wantCode: codes.Unauthenticated,
		},
		{
			desc:     "health check is unauthenticated",
			conf:     sharedConf,
			agents:   testAgents(),
			external: true,
			server:   UnauthenticatedHealthService{},
			wantCode: codes.OK,
		},
		{
			desc:     "internal server ignores agents and accepts a missing token when auth is disabled",
			conf:     gitalycfgauth.Config{},
			agents:   testAgents(),
			external: false,
			wantCode: codes.OK,
		},
		{
			desc:     "internal server requires the shared token when it is set",
			conf:     sharedConf,
			agents:   testAgents(),
			external: false,
			wantCode: codes.Unauthenticated,
		},
		{
			desc:          "internal server accepts the shared token without setting an identity",
			conf:          sharedConf,
			agents:        testAgents(),
			external:      false,
			authorization: v2Authorization(sharedSecretTest, now),
			wantCode:      codes.OK,
		},
		{
			desc:     "no agents: upstream rejects a missing token",
			conf:     sharedConf,
			external: true,
			wantCode: codes.Unauthenticated,
		},
		{
			desc:          "no agents: upstream accepts the shared token without setting an identity",
			conf:          sharedConf,
			external:      true,
			authorization: v2Authorization(sharedSecretTest, now),
			wantCode:      codes.OK,
		},
		{
			desc:     "no agents and no token: upstream accepts everything without identity",
			conf:     gitalycfgauth.Config{},
			external: true,
			wantCode: codes.OK,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			unary, _, err := Interceptors(tc.conf, tc.agents, tc.external)
			require.NoError(t, err)

			ctx := contextWithAuthorization(t, tc.authorization)
			id, found, err := callUnary(t, ctx, unary, tc.server)
			require.Equal(t, tc.wantCode, status.Code(err), "error: %v", err)
			require.Equal(t, tc.wantIdentity, found)
			if tc.wantIdentity {
				require.Equal(t, tc.wantID, id)
			}
		})
	}
}

func TestInterceptors_StreamAgentToken(t *testing.T) {
	t.Parallel()

	_, stream, err := Interceptors(gitalycfgauth.Config{Token: sharedSecretTest}, testAgents(), true)
	require.NoError(t, err)

	t.Run("agent identity reaches the handler", func(t *testing.T) {
		t.Parallel()

		ctx := contextWithAuthorization(t, "Bearer "+agentBSecret)

		var got identity.Identity
		var found bool
		err := stream(nil, fakeServerStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: testMethod},
			func(_ any, ss grpc.ServerStream) error {
				got, found = identity.FromContext(ss.Context())
				return nil
			})

		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, identity.Identity{Name: agentBName, Source: identity.SourceAgent}, got)
	})

	t.Run("wrong token is rejected before the handler", func(t *testing.T) {
		t.Parallel()

		ctx := contextWithAuthorization(t, "Bearer wrong")

		called := false
		err := stream(nil, fakeServerStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: testMethod},
			func(_ any, _ grpc.ServerStream) error {
				called = true
				return nil
			})

		require.Equal(t, codes.Unauthenticated, status.Code(err))
		require.False(t, called)
	})
}

type fakeServerStream struct {
	grpc.ServerStream

	ctx context.Context
}

func (s fakeServerStream) Context() context.Context { return s.ctx }

func TestLoadAgentCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	tokenFile := filepath.Join(dir, "agent-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("  file-secret \n"), 0o600))

	emptyFile := filepath.Join(dir, "empty-token")
	require.NoError(t, os.WriteFile(emptyFile, []byte(" \n"), 0o600))

	multiLineFile := filepath.Join(dir, "multi-line-token")
	require.NoError(t, os.WriteFile(multiLineFile, []byte("line1\nline2\n"), 0o600))

	testCases := []struct {
		desc        string
		sharedToken string
		agents      []config.GraftAgent
		want        []agentCredential
		wantErr     string
	}{
		{
			desc:        "agent token equal to the shared token is rejected",
			sharedToken: "same",
			agents:      []config.GraftAgent{{Name: "a", Token: "same"}},
			wantErr:     "same as auth.token",
		},
		{
			desc:        "agent token different from the shared token is accepted",
			sharedToken: "shared",
			agents:      []config.GraftAgent{{Name: "a", Token: "tok"}},
			want:        []agentCredential{{name: "a", token: "tok"}},
		},
		{
			desc:    "inline token with whitespace is rejected",
			agents:  []config.GraftAgent{{Name: "a", Token: "two words"}},
			wantErr: "whitespace or control characters",
		},
		{
			desc:    "inline token with a control character is rejected",
			agents:  []config.GraftAgent{{Name: "a", Token: "tok\x00"}},
			wantErr: "whitespace or control characters",
		},
		{
			desc:    "token file with an internal newline is rejected",
			agents:  []config.GraftAgent{{Name: "a", TokenFile: multiLineFile}},
			wantErr: "whitespace or control characters",
		},
		{
			desc:   "no agents",
			agents: nil,
			want:   []agentCredential{},
		},
		{
			desc:   "inline token",
			agents: []config.GraftAgent{{Name: "a", Token: "tok"}},
			want:   []agentCredential{{name: "a", token: "tok"}},
		},
		{
			desc:   "token file is read and trimmed",
			agents: []config.GraftAgent{{Name: "a", TokenFile: tokenFile}},
			want:   []agentCredential{{name: "a", token: "file-secret"}},
		},
		{
			desc:    "empty name",
			agents:  []config.GraftAgent{{Name: "", Token: "tok"}},
			wantErr: "name must not be empty",
		},
		{
			desc:    "reserved shared name",
			agents:  []config.GraftAgent{{Name: sharedTokenIdentityName, Token: "tok"}},
			wantErr: "is reserved",
		},
		{
			desc:    "duplicate name",
			agents:  []config.GraftAgent{{Name: "a", Token: "one"}, {Name: "a", Token: "two"}},
			wantErr: "duplicate name",
		},
		{
			desc:    "duplicate token",
			agents:  []config.GraftAgent{{Name: "a", Token: "same"}, {Name: "b", Token: "same"}},
			wantErr: "already used",
		},
		{
			desc:    "token and token file are mutually exclusive",
			agents:  []config.GraftAgent{{Name: "a", Token: "tok", TokenFile: tokenFile}},
			wantErr: "mutually exclusive",
		},
		{
			desc:    "neither token nor token file",
			agents:  []config.GraftAgent{{Name: "a"}},
			wantErr: "either token or token_file must be set",
		},
		{
			desc:    "missing token file",
			agents:  []config.GraftAgent{{Name: "a", TokenFile: filepath.Join(dir, "missing")}},
			wantErr: "reading token file",
		},
		{
			desc:    "empty token file",
			agents:  []config.GraftAgent{{Name: "a", TokenFile: emptyFile}},
			wantErr: "is empty",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			got, err := loadAgentCredentials(tc.sharedToken, tc.agents)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestInterceptors_InvalidAgentConfigFailsAtStartup(t *testing.T) {
	t.Parallel()

	_, _, err := Interceptors(gitalycfgauth.Config{}, []config.GraftAgent{{Name: "a"}}, true)
	require.ErrorContains(t, err, "either token or token_file must be set")

	// The internal server uses upstream authentication, but a broken agent
	// configuration is still reported, so that a Gitaly instance without external
	// listeners does not start with it unnoticed.
	_, _, err = Interceptors(gitalycfgauth.Config{}, []config.GraftAgent{{Name: "a"}}, false)
	require.ErrorContains(t, err, "either token or token_file must be set")

	// An unreadable token file is reported by the internal server as well.
	missing := filepath.Join(t.TempDir(), "missing")
	_, _, err = Interceptors(gitalycfgauth.Config{}, []config.GraftAgent{{Name: "a", TokenFile: missing}}, false)
	require.ErrorContains(t, err, "reading token file")

	// Without agents the internal server is unchanged.
	_, _, err = Interceptors(gitalycfgauth.Config{}, nil, false)
	require.NoError(t, err)

	// An agent with the shared auth.token is reported at startup, on both servers.
	for _, external := range []bool{true, false} {
		_, _, err = Interceptors(gitalycfgauth.Config{Token: "same"}, []config.GraftAgent{{Name: "a", Token: "same"}}, external)
		require.ErrorContains(t, err, "same as auth.token")
	}
}

func TestInterceptors_ConcurrentAuthentication(t *testing.T) {
	t.Parallel()

	unary, _, err := Interceptors(gitalycfgauth.Config{Token: sharedSecretTest}, testAgents(), true)
	require.NoError(t, err)

	const workers = 50
	const iterations = 20

	var wg sync.WaitGroup
	errs := make(chan error, workers*iterations)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			name, secret := agentAName, agentASecret
			if i%2 == 1 {
				name, secret = agentBName, agentBSecret
			}

			for j := 0; j < iterations; j++ {
				ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+secret))

				var got identity.Identity
				_, err := unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: testMethod},
					func(ctx context.Context, _ any) (any, error) {
						got, _ = identity.FromContext(ctx)
						return nil, nil
					})
				if err != nil {
					errs <- err
					continue
				}
				if got.Name != name {
					errs <- fmt.Errorf("worker %d: identity %q, want %q", i, got.Name, name)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	var all []error
	for err := range errs {
		all = append(all, err)
	}
	require.NoError(t, errors.Join(all...))
	assert.Empty(t, all)
}
