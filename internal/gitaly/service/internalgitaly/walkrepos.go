package internalgitaly

import (
	"io/fs"

	"github.com/feder-cr/open_scion/internal/gitaly/storage/walk"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *server) WalkRepos(req *gitalypb.WalkReposRequest, stream gitalypb.InternalGitaly_WalkReposServer) error {
	sendRepo := func(relPath string, gitDirInfo fs.FileInfo) error {
		return stream.Send(&gitalypb.WalkReposResponse{
			RelativePath:     relPath,
			ModificationTime: timestamppb.New(gitDirInfo.ModTime()),
		})
	}

	if err := walk.FindRepositories(stream.Context(), s.locator, req.GetStorageName(), sendRepo); err != nil {
		return structerr.NewInternal("%w", err)
	}

	return nil
}
