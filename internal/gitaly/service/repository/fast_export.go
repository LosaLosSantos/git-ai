package repository

import (
	"bytes"
	"errors"
	"io"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
)

func (s *server) FastExport(
	req *gitalypb.FastExportRequest,
	stream gitalypb.RepositoryService_FastExportServer,
) error {
	ctx := stream.Context()

	if req.GetRepository() == nil {
		return structerr.NewInvalidArgument("repository is not set")
	}

	var stderr bytes.Buffer

	repo := s.localRepoFactory.Build(req.GetRepository())
	cmd, err := repo.Exec(ctx, gitcmd.Command{
		Name: "fast-export",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "--all"},
		},
	}, gitcmd.WithSetupStdout(), gitcmd.WithStderr(&stderr))
	if err != nil {
		return structerr.NewInternal("cmd start failed: %w", err).
			WithMetadata("stderr", stderr.String())
	}

	if err = sendFastExportChunked(cmd, stream); err != nil {
		return structerr.NewInternal("sending chunked response failed: %w", err).
			WithMetadata("stderr", stderr.String())
	}

	return nil
}

func sendFastExportChunked(
	cmd *command.Command,
	stream gitalypb.RepositoryService_FastExportServer,
) error {
	sw := streamio.NewWriter(func(p []byte) error {
		return stream.Send(&gitalypb.FastExportResponse{Data: p})
	})

	for {
		_, err := io.CopyN(sw, cmd, int64(streamio.WriteBufferSize))
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}

	return nil
}
