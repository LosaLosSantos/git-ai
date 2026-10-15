package ssh

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/feder-cr/open_scion/internal/bundleuri"
	"github.com/feder-cr/open_scion/internal/command"
	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/pktline"
	"github.com/feder-cr/open_scion/internal/git/stats"
	"github.com/feder-cr/open_scion/internal/grpc/sidechannel"
	"github.com/feder-cr/open_scion/internal/helper"
	"github.com/feder-cr/open_scion/internal/stream"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

func (s *server) sshUploadPack(ctx context.Context, req *gitalypb.SSHUploadPackWithSidechannelRequest, stdin io.Reader, stdout, stderr io.Writer) (negotiation *stats.PackfileNegotiation, _ int, _ error) {
	repoProto := req.GetRepository()

	repo := s.localRepoFactory.Build(repoProto)
	repoPath, err := repo.Path(ctx)
	if err != nil {
		return nil, 0, err
	}

	gitcmd.WarnIfTooManyBitmaps(ctx, s.logger, s.locator, repoProto.GetStorageName(), repoPath)

	config, err := gitcmd.ConvertConfigOptions(req.GetGitConfigOptions())
	if err != nil {
		return nil, 0, err
	}

	var wg sync.WaitGroup
	pr, pw := io.Pipe()
	defer func() {
		pw.Close()
		wg.Wait()
	}()

	stdin = io.TeeReader(stdin, pw)

	wg.Add(1)
	go func() {
		defer func() {
			wg.Done()
			pr.Close()
		}()

		var opts []stats.ParsePackfileNegotiationOption
		if featureflag.LsRefsPipelineRefInstrumentation.IsEnabled(ctx) {
			opts = append(opts, stats.WithLsRefsPipelineRefPrefixes())
		}

		stats, errIgnore := stats.ParsePackfileNegotiation(pr, opts...)
		negotiation = &stats
		if errIgnore != nil {
			s.logger.WithError(errIgnore).DebugContext(ctx, "failed parsing packfile negotiation")
			return
		}
		stats.UpdateMetrics(s.packfileNegotiationMetrics, s.packfileNegotiationDeepenMetrics)
		stats.UpdateLsRefsPipelineRefLogFields(ctx)
		stats.UpdateLogFields(ctx)
	}()

	if s.bundleURIManager != nil {
		// Bundle generation is an optimization that is transparent to users.
		// If it fails, we log the error but continue with the regular upload-pack
		// operation without the bundle optimization.
		// If successful, a goroutine is spawned to generate the bundle, in which case
		// the bundle generation becomes independent of the RPC request.
		if err = s.bundleURIManager.GenerateWithStrategy(ctx, repo); err != nil {
			s.logger.WithError(err).Error("failed generating bundle")
		}
		config = append(config, s.bundleURIManager.UploadPackGitConfig(ctx, req.GetRepository())...)
	} else {
		config = append(config, bundleuri.CapabilitiesGitConfig(ctx, false)...)
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("detecting object hash: %w", err)
	}

	commandOpts := []gitcmd.CmdOpt{
		gitcmd.WithGitProtocol(s.logger, req),
		gitcmd.WithConfig(config...),
		gitcmd.WithPackObjectsHookEnv(objectHash, repoProto, "ssh"),
	}

	if s.cfg.Hooks.PackObjectsHookMaxProc > 0 {
		commandOpts = append(commandOpts, gitcmd.WithEnv("GOMAXPROCS="+strconv.Itoa(int(s.cfg.Hooks.PackObjectsHookMaxProc))))
	}

	timeoutTicker := s.uploadPackRequestTimeoutTickerFactory()

	// upload-pack negotiation is terminated by either a flush, or the "done"
	// packet: https://github.com/git/git/blob/v2.20.0/Documentation/technical/pack-protocol.txt#L335
	//
	// "flush" tells the server it can terminate, while "done" tells it to start
	// generating a packfile. Add a timeout to the second case to mitigate
	// use-after-check attacks.
	if err := s.runUploadCommand(ctx, repo, stdin, stdout, stderr, timeoutTicker, pktline.PktDone(), gitcmd.Command{
		Name: "upload-pack",
		Args: []string{repoPath},
	}, commandOpts...); err != nil {
		status, _ := command.ExitStatus(err)
		return nil, status, fmt.Errorf("running upload-pack: %w", err)
	}

	return nil, 0, nil
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

func (s *server) SSHUploadPackWithSidechannel(ctx context.Context, req *gitalypb.SSHUploadPackWithSidechannelRequest) (*gitalypb.SSHUploadPackWithSidechannelResponse, error) {
	conn, err := sidechannel.OpenSidechannel(ctx)
	if err != nil {
		return nil, structerr.NewInternal("opennig sidechannel: %w", err)
	}
	defer conn.Close()

	// Count all bytes written back to the client so we can report the
	// client-facing transfer size, labeled by clone type.
	counter := &helper.CountingWriter{W: conn}
	sidebandWriter := pktline.NewSidebandWriter(counter)
	stdout := sidebandWriter.Writer(stream.BandStdout)
	stderr := sidebandWriter.Writer(stream.BandStderr)
	stats, _, err := s.sshUploadPack(ctx, req, conn, stdout, stderr)
	if err != nil {
		return nil, structerr.NewInternal("%w", err)
	}

	s.recordServedBytes(stats, counter.N)

	if err := conn.Close(); err != nil {
		return nil, structerr.NewInternal("close sidechannel: %w", err)
	}

	return &gitalypb.SSHUploadPackWithSidechannelResponse{
		PackfileNegotiationStatistics: stats.ToProto(),
	}, nil
}
