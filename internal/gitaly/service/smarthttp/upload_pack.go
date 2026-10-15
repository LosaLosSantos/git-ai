package smarthttp

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/feder-cr/open_scion/internal/bundleuri"
	"github.com/feder-cr/open_scion/internal/command"
	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/stats"
	"github.com/feder-cr/open_scion/internal/grpc/sidechannel"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

func (s *server) PostUploadPackWithSidechannel(ctx context.Context, req *gitalypb.PostUploadPackWithSidechannelRequest) (*gitalypb.PostUploadPackWithSidechannelResponse, error) {
	negotiation, err := s.uploadPackWithSidechannel(ctx, req, false)
	if err != nil {
		return nil, err
	}

	return &gitalypb.PostUploadPackWithSidechannelResponse{
		PackfileNegotiationStatistics: negotiation.ToProto(),
	}, nil
}

// uploadPackWithSidechannel validates the request, opens a sidechannel, runs git-upload-pack(1)
// against it, and closes the sidechannel. It is shared by PostUploadPackWithSidechannel and
// AdvertiseBundleURI.
//
// advertiseBundleURI forces uploadpack.advertiseBundleURIs=true so that git-upload-pack(1) answers a
// `command=bundle-uri` request instead of dying with "invalid command", regardless of the
// repository's configuration or the bundle_uri feature flag.
func (s *server) uploadPackWithSidechannel(ctx context.Context, req *gitalypb.PostUploadPackWithSidechannelRequest, advertiseBundleURI bool) (*stats.PackfileNegotiation, error) {
	repoPath, gitConfig, err := s.validateUploadPackRequest(ctx, req)
	if err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	var sidechannelRetryableError sidechannel.RetryableError
	conn, err := sidechannel.OpenSidechannel(ctx)
	if err != nil {
		if errors.As(err, &sidechannelRetryableError) {
			// Clients of PostUploadPackWithSidechannel are configured to retry the RPC upon receiving
			// Unavailable, so it should be OK to return it in this case.
			//nolint:gitaly-linters
			return nil, structerr.NewUnavailable("open sidechannel: %w", err)
		}
		return nil, structerr.NewInternal("open sidechannel: %w", err)
	}
	defer conn.Close()

	negotiation, err := s.runUploadPack(ctx, req, repoPath, gitConfig, conn, conn, advertiseBundleURI)
	if err != nil {
		return nil, structerr.NewInternal("running upload-pack: %w", err)
	}

	if err := conn.Close(); err != nil {
		return nil, structerr.NewInternal("close sidechannel connection: %w", err)
	}

	return negotiation, nil
}

type statsCollector struct {
	c       io.Closer
	statsCh chan stats.PackfileNegotiation
}

func (sc *statsCollector) finish() *stats.PackfileNegotiation {
	sc.c.Close()
	stats := <-sc.statsCh
	return &stats
}

func (s *server) runStatsCollector(ctx context.Context, r io.Reader) (io.Reader, *statsCollector) {
	pr, pw := io.Pipe()
	sc := &statsCollector{
		c:       pw,
		statsCh: make(chan stats.PackfileNegotiation, 1),
	}

	go func() {
		defer close(sc.statsCh)

		var opts []stats.ParsePackfileNegotiationOption
		if featureflag.LsRefsPipelineRefInstrumentation.IsEnabled(ctx) {
			opts = append(opts, stats.WithLsRefsPipelineRefPrefixes())
		}

		stats, err := stats.ParsePackfileNegotiation(pr, opts...)
		if err != nil {
			s.logger.WithError(err).DebugContext(ctx, "failed parsing packfile negotiation")
			return
		}
		stats.UpdateMetrics(s.packfileNegotiationMetrics, s.packfileNegotiationDeepenMetrics)
		stats.UpdateLsRefsPipelineRefLogFields(ctx)
		stats.UpdateLogFields(ctx)

		sc.statsCh <- stats
	}()

	return io.TeeReader(r, pw), sc
}

// recordServedBytes records the number of bytes served to the client during
// upload-pack, labeled by clone type. Requests that do not ask for any objects
// (e.g. a protocol v2 ls-refs round trip) are skipped so the metric only
// reflects actual pack-producing fetches and clones.
func (s *server) recordServedBytes(negotiation *stats.PackfileNegotiation, served int64) {
	if negotiation == nil || negotiation.Wants == 0 {
		return
	}

	s.uploadPackServedBytesMetrics.WithLabelValues(negotiation.CloneType()).Add(float64(served))
}

func (s *server) validateUploadPackRequest(ctx context.Context, req *gitalypb.PostUploadPackWithSidechannelRequest) (string, []gitcmd.ConfigPair, error) {
	repository := req.GetRepository()
	if err := s.locator.ValidateRepository(ctx, repository); err != nil {
		return "", nil, err
	}
	repoPath, err := s.locator.GetRepoPath(ctx, repository)
	if err != nil {
		return "", nil, err
	}

	gitcmd.WarnIfTooManyBitmaps(ctx, s.logger, s.locator, repository.GetStorageName(), repoPath)

	config, err := gitcmd.ConvertConfigOptions(req.GetGitConfigOptions())
	if err != nil {
		return "", nil, err
	}

	return repoPath, config, nil
}

func (s *server) runUploadPack(ctx context.Context, req *gitalypb.PostUploadPackWithSidechannelRequest, repoPath string, gitConfig []gitcmd.ConfigPair, stdin io.Reader, stdout io.Writer, advertiseBundleURI bool) (stats *stats.PackfileNegotiation, _ error) {
	h := sha1.New()

	stdin = io.TeeReader(stdin, h)
	stdin, collector := s.runStatsCollector(ctx, stdin)
	defer func() {
		if stats == nil {
			stats = collector.finish()
		}
	}()

	repo := s.localRepoFactory.Build(req.GetRepository())
	if s.bundleURIManager != nil {
		// Bundle generation is an optimization that is transparent to users.
		// If it fails, we log the error but continue with the regular upload-pack
		// operation without the bundle optimization.
		// If successful, a goroutine is spawned to generate the bundle, in which case
		// the bundle generation becomes independent of the RPC request.
		if err := s.bundleURIManager.GenerateWithStrategy(ctx, repo); err != nil {
			s.logger.WithError(err).Error("failed generating bundle")
		}
		gitConfig = append(gitConfig, s.bundleURIManager.UploadPackGitConfig(ctx, req.GetRepository())...)
	} else {
		gitConfig = append(gitConfig, bundleuri.CapabilitiesGitConfig(ctx, false)...)
	}

	if advertiseBundleURI {
		// Appended last so it wins over any uploadpack.advertiseBundleURIs value already in
		// gitConfig: git applies repeated -c options in order, and the last one for a given key
		// takes effect. A raw ConfigPair is used here instead of bundleuri.CapabilitiesGitConfig,
		// which would force the value back to false while the bundle_uri feature flag is disabled.
		gitConfig = append(gitConfig, gitcmd.ConfigPair{
			Key:   "uploadpack.advertiseBundleURIs",
			Value: "true",
		})
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	commandOpts := []gitcmd.CmdOpt{
		gitcmd.WithStdin(stdin),
		gitcmd.WithSetupStdout(),
		gitcmd.WithGitProtocol(s.logger, req),
		gitcmd.WithConfig(gitConfig...),
		gitcmd.WithPackObjectsHookEnv(objectHash, req.GetRepository(), "http"),
	}

	if s.cfg.Hooks.PackObjectsHookMaxProc > 0 {
		commandOpts = append(commandOpts, gitcmd.WithEnv("GOMAXPROCS="+strconv.Itoa(int(s.cfg.Hooks.PackObjectsHookMaxProc))))
	}

	cmd, err := repo.Exec(ctx, gitcmd.Command{
		Name:  "upload-pack",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "--stateless-rpc"}},
		Args:  []string{repoPath},
	}, commandOpts...)
	if err != nil {
		return nil, structerr.NewFailedPrecondition("spawning upload-pack: %w", err)
	}

	// Use a custom buffer size to minimize the number of system calls.
	respBytes, err := io.CopyBuffer(stdout, cmd, make([]byte, 64*1024))
	if err != nil {
		return nil, structerr.NewFailedPrecondition("copying stdout from upload-pack: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		stats = collector.finish()
		if _, ok := command.ExitStatus(err); ok && stats.Deepen != "" {
			// We have seen a 'deepen' message in the request. It is expected that
			// git-upload-pack has a non-zero exit status: don't treat this as an
			// error.
			s.recordServedBytes(stats, respBytes)
			return stats, nil
		}

		return nil, structerr.NewFailedPrecondition("waiting for upload-pack: %w", err)
	}

	stats = collector.finish()
	s.recordServedBytes(stats, respBytes)

	s.logger.WithField("request_sha", fmt.Sprintf("%x", h.Sum(nil))).WithField("response_bytes", respBytes).InfoContext(ctx, "request details")
	return stats, nil
}
