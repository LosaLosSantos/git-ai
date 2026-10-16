package remote

import (
	"bytes"
	"context"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func (s *server) FindRemoteRepository(ctx context.Context, req *gitalypb.FindRemoteRepositoryRequest) (*gitalypb.FindRemoteRepositoryResponse, error) {
	remoteURL := req.GetRemote()

	if remoteURL == "" {
		return nil, structerr.NewInvalidArgument("empty remote can't be checked.")
	}

	var config []gitcmd.ConfigPair

	if resolvedAddress := req.GetResolvedAddress(); resolvedAddress != "" {
		modifiedURL, resolveConfig, err := gitcmd.GetURLAndResolveConfig(remoteURL, resolvedAddress)
		if err != nil {
			return nil, structerr.NewInvalidArgument("couldn't get curloptResolve config: %w", err)
		}

		remoteURL = modifiedURL
		config = append(config, resolveConfig...)
	}

	var output bytes.Buffer
	cmd, err := s.gitCmdFactory.NewWithoutRepo(ctx,
		gitcmd.Command{
			Name: "ls-remote",
			Args: []string{
				remoteURL,
				"HEAD",
			},
		},
		gitcmd.WithConfigEnv(config...),
		gitcmd.WithStdout(&output),
	)
	if err != nil {
		return nil, structerr.NewInternal("error executing git command: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return &gitalypb.FindRemoteRepositoryResponse{Exists: false}, nil
	}

	return &gitalypb.FindRemoteRepositoryResponse{Exists: remoteHeadExists(output.Bytes())}, nil
}

// remoteHeadExists reports whether the output of `git ls-remote <remote> HEAD` names the remote's
// HEAD with an object ID of a known object hash.
func remoteHeadExists(output []byte) bool {
	// git-ls-remote(1) matches the pattern against the tail of each reference name, so the output
	// lists HEAD and also every reference whose last component is HEAD:
	//
	//	58fbff2e0d3b620f591a748c158799ead87b51cd	HEAD\n
	//	58fbff2e0d3b620f591a748c158799ead87b51cd	refs/remotes/origin/HEAD\n
	//
	// Only the line for HEAD itself counts. A remote whose HEAD is unborn lists no such line,
	// even when it has other references ending in HEAD.
	for line := range bytes.Lines(output) {
		objectID, refname, ok := bytes.Cut(line, []byte("\t"))
		if !ok || !bytes.Equal(refname, []byte("HEAD\n")) {
			continue
		}

		// We have no way to ask the remote's object format via git-ls-remote(1), so all we can do
		// is to verify that the object hash matches something we know.
		n := len(objectID)
		return n == git.ObjectHashSHA1.EncodedLen() || n == git.ObjectHashSHA256.EncodedLen()
	}

	return false
}
