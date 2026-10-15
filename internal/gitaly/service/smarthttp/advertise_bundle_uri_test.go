package smarthttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/open_scion/internal/bundleuri"
	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/gittest"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/feder-cr/open_scion/internal/gitaly/storage/mode"
	"github.com/feder-cr/open_scion/internal/grpc/sidechannel"
	"github.com/feder-cr/open_scion/internal/helper/duration"
	"github.com/feder-cr/open_scion/internal/testhelper"
	"github.com/feder-cr/open_scion/internal/testhelper/testcfg"
	"github.com/feder-cr/open_scion/internal/testhelper/testserver"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

const bundleURIBaseURL = "https://example.com"

func TestServer_AdvertiseBundleURI(t *testing.T) {
	t.Parallel()

	ctx := bundleURIContext(t)
	cfg, repo := setupAdvertiseBundleURIServer(t, ctx, testcfg.Build(t))

	responseBuffer, err := makeAdvertiseBundleURIRequest(t, ctx, cfg.SocketPath, cfg.Auth.Token,
		&gitalypb.AdvertiseBundleURIRequest{Repository: repo, GitProtocol: gitcmd.ProtocolV2},
		bundleURIRequestBody(t),
	)
	require.NoError(t, err)
	require.Contains(t, responseBuffer.String(), "bundle.default.uri="+bundleURIBaseURL)
}

// TestServer_AdvertiseBundleURI_withoutBundle verifies that a `command=bundle-uri` request is
// answered even when the repository has no bundle and the bundle_uri feature flag is disabled.
// git-upload-pack(1) dies on a command whose capability is not advertised, so without the
// capability being force-enabled the RPC would fail and Workhorse would turn that into an HTTP 500.
// The client instead receives an empty bundle list, upon which it falls back to a normal fetch.
func TestServer_AdvertiseBundleURI_withoutBundle(t *testing.T) {
	t.Parallel()

	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.BundleURI, false)

	cfg := testcfg.Build(t)
	cfg.SocketPath = startSmartHTTPServer(t, cfg).Address()

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	responseBuffer, err := makeAdvertiseBundleURIRequest(t, ctx, cfg.SocketPath, cfg.Auth.Token,
		&gitalypb.AdvertiseBundleURIRequest{Repository: repoProto, GitProtocol: gitcmd.ProtocolV2},
		bundleURIRequestBody(t),
	)
	require.NoError(t, err)
	// A flush packet with no preceding `bundle.*` lines is the empty bundle list.
	require.Equal(t, "0000", responseBuffer.String())
}

// TestServer_AdvertiseBundleURI_separateConcurrencyLimit verifies the reason this RPC exists: it
// has its own concurrency-limiting cost class and is thus unaffected by a saturated
// PostUploadPackWithSidechannel limit.
func TestServer_AdvertiseBundleURI_separateConcurrencyLimit(t *testing.T) {
	t.Parallel()

	ctx := bundleURIContext(t)

	cfg := testcfg.Build(t)
	cfg.Concurrency = []config.Concurrency{
		{
			RPC: "/gitaly.SmartHTTPService/PostUploadPackWithSidechannel",
			ConcurrencyLimits: config.ConcurrencyLimits{
				MaxConcurrency: 1,
				MaxQueueSize:   1,
				MaxQueueWait:   duration.Duration(100 * time.Millisecond),
			},
		},
	}
	cfg, repo := setupAdvertiseBundleURIServer(t, ctx, cfg)

	// Occupy the single concurrency slot of PostUploadPackWithSidechannel so that its limit is
	// saturated for the duration of this test.
	<-saturatePostUploadPackLimit(t, ctx, cfg, repo)

	// A second PostUploadPackWithSidechannel request has to wait in the queue and is rejected
	// once it exceeds the maximum queueing time.
	_, err := makePostUploadPackWithSidechannelRequest(t, ctx, cfg.SocketPath, cfg.Auth.Token,
		&gitalypb.PostUploadPackWithSidechannelRequest{Repository: repo, GitProtocol: gitcmd.ProtocolV2},
		bundleURIRequestBody(t),
	)
	testhelper.RequireGrpcCode(t, err, codes.ResourceExhausted)

	// AdvertiseBundleURI has no limit configured, so it is served regardless.
	responseBuffer, err := makeAdvertiseBundleURIRequest(t, ctx, cfg.SocketPath, cfg.Auth.Token,
		&gitalypb.AdvertiseBundleURIRequest{Repository: repo, GitProtocol: gitcmd.ProtocolV2},
		bundleURIRequestBody(t),
	)
	require.NoError(t, err)
	require.Contains(t, responseBuffer.String(), "bundle.default.uri="+bundleURIBaseURL)
}

// bundleURIContext returns a context with the feature flags required to advertise bundle URIs.
func bundleURIContext(t *testing.T) context.Context {
	t.Helper()

	ctx := featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.BundleURI, true)

	return featureflag.ContextWithFeatureFlag(ctx, featureflag.BundleGeneration, true)
}

// setupAdvertiseBundleURIServer starts a Gitaly server that advertises bundle URIs and returns the
// adapted configuration along with a repository that has a bundle generated for it.
func setupAdvertiseBundleURIServer(t *testing.T, ctx context.Context, cfg config.Cfg) (config.Cfg, *gitalypb.Repository) {
	t.Helper()

	keyPath := filepath.Join(testhelper.TempDir(t), "secret.key")
	require.NoError(t, os.WriteFile(keyPath, []byte("super-secret-key"), mode.File))

	sink, err := bundleuri.NewSink(ctx, "file://"+testhelper.TempDir(t)+
		"?base_url="+bundleURIBaseURL+"&no_tmp_dir=true&secret_key_path="+keyPath)
	require.NoError(t, err)

	strategy := bundleuri.NewSimpleStrategy(true)
	manager, err := bundleuri.NewGenerationManager(ctx, sink, testhelper.NewLogger(t), nil, strategy)
	require.NoError(t, err)

	cfg.SocketPath = startSmartHTTPServerWithOptions(t, cfg, nil, []testserver.GitalyServerOpt{
		testserver.WithBundleURIStrategy(strategy),
		testserver.WithBundleURISink(sink),
	}).Address()

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	require.NoError(t, manager.Generate(ctx, localrepo.NewTestRepo(t, cfg, repoProto)))

	return cfg, repoProto
}

// bundleURIRequestBody returns the request a Git client sends for the protocol v2
// `command=bundle-uri` command.
func bundleURIRequestBody(t *testing.T) io.Reader {
	t.Helper()

	body := &bytes.Buffer{}
	gittest.WritePktlineString(t, body, "command=bundle-uri\n")
	gittest.WritePktlineString(t, body, fmt.Sprintf("object-format=%s\n", gittest.DefaultObjectHash.Format))
	gittest.WritePktlineFlush(t, body)

	return body
}

// saturatePostUploadPackLimit starts a PostUploadPackWithSidechannel request that never sends its
// request body and thus keeps git-upload-pack(1) waiting. The returned channel is closed once the
// request has passed the concurrency limiter and thus occupies a concurrency slot, which it does
// until the test finishes.
func saturatePostUploadPackLimit(t *testing.T, ctx context.Context, cfg config.Cfg, repo *gitalypb.Repository) <-chan struct{} {
	t.Helper()

	registry := sidechannel.NewRegistry()
	conn := dialSmartHTTPServerWithSidechannel(t, cfg.SocketPath, cfg.Auth.Token, registry)
	client := gitalypb.NewSmartHTTPServiceClient(conn)

	occupied := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})

	ctxOut, waiter := sidechannel.RegisterSidechannel(ctx, registry, func(sideConn *sidechannel.ClientConn) error {
		// The sidechannel is only opened by the handler, which runs after the concurrency
		// limiter has admitted the request.
		close(occupied)

		// Keep git-upload-pack(1) waiting for its request so that the request stays in
		// flight and keeps holding the concurrency slot.
		<-release

		return sideConn.CloseWrite()
	})

	go func() {
		defer close(finished)

		// Errors are ignored: the request is torn down at the end of the test without
		// ever having sent a valid request to git-upload-pack(1).
		//nolint:errcheck
		client.PostUploadPackWithSidechannel(ctxOut, &gitalypb.PostUploadPackWithSidechannelRequest{
			Repository:  repo,
			GitProtocol: gitcmd.ProtocolV2,
		})
		//nolint:errcheck
		waiter.Close()
	}()

	t.Cleanup(func() {
		close(release)
		<-finished
		testhelper.MustClose(t, conn)
	})

	return occupied
}

func makeAdvertiseBundleURIRequest(t *testing.T, ctx context.Context, serverSocketPath, token string, in *gitalypb.AdvertiseBundleURIRequest, body io.Reader) (*bytes.Buffer, error) {
	t.Helper()

	registry := sidechannel.NewRegistry()
	conn := dialSmartHTTPServerWithSidechannel(t, serverSocketPath, token, registry)
	client := gitalypb.NewSmartHTTPServiceClient(conn)
	defer testhelper.MustClose(t, conn)

	responseBuffer := &bytes.Buffer{}
	ctxOut, waiter := sidechannel.RegisterSidechannel(ctx, registry, func(sideConn *sidechannel.ClientConn) error {
		var wg sync.WaitGroup
		defer wg.Wait()

		wg.Add(1)
		errC := make(chan error, 1)
		go func() {
			defer wg.Done()
			_, err := io.Copy(responseBuffer, sideConn)
			errC <- err
		}()

		if body != nil {
			if _, err := io.Copy(sideConn, body); err != nil {
				return err
			}
		}

		if err := sideConn.CloseWrite(); err != nil {
			return err
		}

		return <-errC
	})

	_, err := client.AdvertiseBundleURI(ctxOut, in)
	if err == nil {
		testhelper.MustClose(t, waiter)
	} else if err := waiter.Close(); err != nil && !errors.Is(err, sidechannel.ErrCallbackDidNotRun) {
		// When the request failed the sidechannel may not even have been used, so we need
		// to catch the `ErrCallbackDidNotRun` error here.
		require.NoError(t, err)
	}

	return responseBuffer, err
}
