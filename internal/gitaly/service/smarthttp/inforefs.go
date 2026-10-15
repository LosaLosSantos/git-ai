package smarthttp

import (
	"context"
	"fmt"
	"io"

	"github.com/feder-cr/open_scion/internal/bundleuri"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/localrepo"
	"github.com/feder-cr/open_scion/internal/git/pktline"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/streamio"
)

const (
	uploadPackSvc  = "upload-pack"
	receivePackSvc = "receive-pack"
)

func (s *server) InfoRefsUploadPack(in *gitalypb.InfoRefsRequest, stream gitalypb.SmartHTTPService_InfoRefsUploadPackServer) error {
	repository := in.GetRepository()
	if err := s.locator.ValidateRepository(stream.Context(), repository); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	repo := s.localRepoFactory.Build(in.GetRepository())
	repoPath, err := repo.Path(stream.Context())
	if err != nil {
		return err
	}

	w := streamio.NewWriter(func(p []byte) error {
		return stream.Send(&gitalypb.InfoRefsResponse{Data: p})
	})

	return s.infoRefCache.tryCache(stream.Context(), in, w, func(w io.Writer) error {
		return s.handleInfoRefs(stream.Context(), uploadPackSvc, repoPath, repo, in, w)
	})
}

func (s *server) InfoRefsReceivePack(in *gitalypb.InfoRefsRequest, stream gitalypb.SmartHTTPService_InfoRefsReceivePackServer) error {
	repository := in.GetRepository()
	if err := s.locator.ValidateRepository(stream.Context(), repository); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	repo := s.localRepoFactory.Build(in.GetRepository())
	repoPath, err := repo.Path(stream.Context())
	if err != nil {
		return err
	}
	w := streamio.NewWriter(func(p []byte) error {
		return stream.Send(&gitalypb.InfoRefsResponse{Data: p})
	})
	return s.handleInfoRefs(stream.Context(), receivePackSvc, repoPath, repo, in, w)
}

func (s *server) handleInfoRefs(ctx context.Context, service, repoPath string, repo *localrepo.Repo, req *gitalypb.InfoRefsRequest, w io.Writer) error {
	s.logger.WithFields(log.Fields{
		"service": service,
	}).DebugContext(ctx, "handleInfoRefs")

	cmdOpts := []gitcmd.CmdOpt{gitcmd.WithGitProtocol(s.logger, req), gitcmd.WithStdout(w)}
	if service == "receive-pack" {
		cmdOpts = append(cmdOpts, gitcmd.WithDisabledHooks())
	}

	gitConfig, err := gitcmd.ConvertConfigOptions(req.GetGitConfigOptions())
	if err != nil {
		return err
	}
	if s.bundleURIManager != nil {
		gitConfig = append(gitConfig, s.bundleURIManager.UploadPackGitConfig(ctx, req.GetRepository())...)
	} else {
		gitConfig = append(gitConfig, bundleuri.CapabilitiesGitConfig(ctx, false)...)
	}

	cmdOpts = append(cmdOpts, gitcmd.WithConfig(gitConfig...))

	if _, err := pktline.WriteString(w, fmt.Sprintf("# service=git-%s\n", service)); err != nil {
		return structerr.NewInternal("pktLine: %w", err)
	}

	if err := pktline.WriteFlush(w); err != nil {
		return structerr.NewInternal("pktFlush: %w", err)
	}

	cmd, err := repo.Exec(ctx, gitcmd.Command{
		Name:  service,
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "--stateless-rpc"}, gitcmd.Flag{Name: "--advertise-refs"}},
		Args:  []string{repoPath},
	}, cmdOpts...)
	if err != nil {
		return structerr.NewInternal("cmd: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return structerr.NewInternal("wait: %w", err)
	}

	return nil
}
