package smarthttp

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/feder-cr/open_scion/internal/featureflag"
	"github.com/feder-cr/open_scion/internal/git/gitcmd"
	"github.com/feder-cr/open_scion/internal/git/stats"
	"github.com/feder-cr/open_scion/internal/gitaly/storage"
	"github.com/feder-cr/open_scion/internal/gitaly/transaction"
	"github.com/feder-cr/open_scion/internal/log"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/internal/transaction/voting"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/feder-cr/open_scion/streamio"
)

func (s *server) PostReceivePack(stream gitalypb.SmartHTTPService_PostReceivePackServer) error {
	ctx := stream.Context()
	req, err := stream.Recv() // First request contains only Repository and GlId
	if err != nil {
		return err
	}

	s.logger.WithFields(log.Fields{
		"GlID":             req.GetGlId(),
		"GlRepository":     req.GetGlRepository(),
		"GlUsername":       req.GetGlUsername(),
		"GitConfigOptions": req.GetGitConfigOptions(),
	}).DebugContext(ctx, "PostReceivePack")

	if err := validateReceivePackRequest(ctx, s.locator, req); err != nil {
		return err
	}

	if err := s.postReceivePack(stream, req); err != nil {
		return structerr.NewInternal("%w", err)
	}

	// In cases where all reference updates are rejected by git-receive-pack(1), we would end up
	// with no transactional votes at all. This would lead to scheduling
	// replication jobs, which wouldn't accomplish anything since no refs
	// were updated.
	// To prevent replication jobs from being unnecessarily created, do a
	// final vote which concludes this RPC to ensure there's always at least
	// one vote. In case there was diverging behaviour in git-receive-pack(1)
	// which led to a different outcome across voters, then this final vote
	// would fail because the sequence of votes would be different.
	if err := transaction.VoteOnContext(ctx, s.txManager, voting.Vote{}, voting.Committed); err != nil {
		// When the pre-receive hook failed, git-receive-pack(1) exits with code 0.
		// It's arguable whether this is the expected behavior, but anyhow it means
		// cmd.Wait() did not error out. On the other hand, the gitaly-hooks command did
		// stop the transaction upon failure. So this final vote fails.
		// To avoid this error being presented to the end user, ignore it when the
		// transaction was stopped.
		if !errors.Is(err, transaction.ErrTransactionStopped) {
			return structerr.NewAborted("final transactional vote: %w", err)
		}
	}

	return nil
}

func (s *server) postReceivePack(
	stream gitalypb.SmartHTTPService_PostReceivePackServer,
	req *gitalypb.PostReceivePackRequest,
) (returnedErr error) {
	ctx := stream.Context()

	stdin := streamio.NewReader(func() ([]byte, error) {
		resp, err := stream.Recv()
		return resp.GetData(), err
	})

	if featureflag.ReceivePackStats.IsEnabled(ctx) {
		// The deferred call runs after cmd.Wait() has drained stdin, so the
		// capture is complete by the time it is read.
		statsCapture := &stats.PrefixCapture{}
		defer func() {
			stats.RecordReceivePack(ctx, s.logger, statsCapture.Bytes(), s.receivePackObjectsMetrics)
		}()
		stdin = io.TeeReader(stdin, statsCapture)
	}

	stdout := streamio.NewWriter(func(p []byte) error {
		return stream.Send(&gitalypb.PostReceivePackResponse{Data: p})
	})

	repo := s.localRepoFactory.Build(req.GetRepository())

	repoPath, err := repo.Path(ctx)
	if err != nil {
		return err
	}

	config, err := gitcmd.ConvertConfigOptions(req.GetGitConfigOptions())
	if err != nil {
		return err
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}

	cmd, err := repo.Exec(ctx,
		gitcmd.Command{
			Name:  "receive-pack",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--stateless-rpc"}},
			Args:  []string{repoPath},
		},
		gitcmd.WithStdin(stdin),
		gitcmd.WithStdout(stdout),
		gitcmd.WithReceivePackHooks(objectHash, req, "http"),
		gitcmd.WithGitProtocol(s.logger, req),
		gitcmd.WithConfig(config...),
	)
	if err != nil {
		return structerr.NewFailedPrecondition("spawning receive-pack: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return structerr.NewFailedPrecondition("waiting for receive-pack: %w", err)
	}

	return nil
}

func validateReceivePackRequest(ctx context.Context, locator storage.Locator, req *gitalypb.PostReceivePackRequest) error {
	if req.GetGlId() == "" {
		return structerr.NewInvalidArgument("empty GlId")
	}
	if req.Data != nil {
		return structerr.NewInvalidArgument("non-empty Data")
	}
	if err := locator.ValidateRepository(ctx, req.GetRepository()); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	return nil
}
