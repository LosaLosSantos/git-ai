package hook

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"

	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
)

func (s *server) ReceiveReportHook(stream gitalypb.HookService_ReceiveReportHookServer) error {
	firstRequest, err := stream.Recv()
	if err != nil {
		return structerr.NewInternal("receiving first request: %w", err)
	}

	if err := s.locator.ValidateRepository(stream.Context(), firstRequest.GetRepository()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}
	repository := firstRequest.GetRepository()

	stdin := streamio.NewReader(func() ([]byte, error) {
		req, err := stream.Recv()
		return req.GetStdin(), err
	})

	var m sync.Mutex
	stdout := streamio.NewSyncWriter(&m, func(p []byte) error {
		return stream.Send(&gitalypb.ReceiveReportHookResponse{Stdout: p})
	})
	stderr := streamio.NewSyncWriter(&m, func(p []byte) error {
		return stream.Send(&gitalypb.ReceiveReportHookResponse{Stderr: p})
	})

	if err := s.manager.ReceiveReportHook(
		stream.Context(),
		repository,
		firstRequest.GetGitPushOptions(),
		firstRequest.GetEnvironmentVariables(),
		stdin,
		stdout,
		stderr,
	); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return receiveReportHookResponse(stream, int32(exitError.ExitCode()), "")
		}

		return receiveReportHookResponse(stream, 1, fmt.Sprintf("%s", err))
	}

	return receiveReportHookResponse(stream, 0, "")
}

func receiveReportHookResponse(stream gitalypb.HookService_ReceiveReportHookServer, code int32, stderr string) error {
	var err error

	// if the error is not empty, we use `streamio.NewSyncWriter` to
	// chunk the output
	if len(stderr) > 0 {
		var m sync.Mutex
		errOutput := streamio.NewSyncWriter(&m, func(p []byte) error {
			return stream.Send(&gitalypb.ReceiveReportHookResponse{
				Stderr:     p,
				ExitStatus: &gitalypb.ExitStatus{Value: code},
			})
		})

		_, err = errOutput.Write([]byte(stderr))
	} else {
		err = stream.Send(&gitalypb.ReceiveReportHookResponse{
			Stderr:     []byte(stderr),
			ExitStatus: &gitalypb.ExitStatus{Value: code},
		})
	}

	if err != nil {
		return structerr.NewInternal("sending response: %w", err)
	}

	return nil
}
