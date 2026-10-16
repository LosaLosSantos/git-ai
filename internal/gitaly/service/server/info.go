package server

import (
	"context"
	"os"

	"github.com/feder-cr/git-ai/internal/gitaly/storage/mdfile"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode/permission"
	"github.com/feder-cr/git-ai/internal/helper/fstype"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/version"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func (s *server) ServerInfo(ctx context.Context, in *gitalypb.ServerInfoRequest) (*gitalypb.ServerInfoResponse, error) {
	gitVersion, err := s.gitCmdFactory.GitVersion(ctx)
	if err != nil {
		return nil, structerr.NewInternal("%w", err)
	}

	var storageStatuses []*gitalypb.ServerInfoResponse_StorageStatus
	for _, shard := range s.storages {
		readable, writeable := shardCheck(shard.Path)
		fsType := fstype.FileSystem(shard.Path)

		gitalyMetadata, err := mdfile.ReadMetadataFile(shard.Path)
		if err != nil {
			s.logger.WithField("storage", shard).WithError(err).ErrorContext(ctx, "reading gitaly metadata file")
		}

		storageStatuses = append(storageStatuses, &gitalypb.ServerInfoResponse_StorageStatus{
			StorageName:       shard.Name,
			ReplicationFactor: 1, // gitaly is always treated as a single replica
			Readable:          readable,
			Writeable:         writeable,
			FsType:            fsType,
			FilesystemId:      gitalyMetadata.GitalyFilesystemID,
		})
	}

	return &gitalypb.ServerInfoResponse{
		ServerVersion:   version.GetVersion(),
		GitVersion:      gitVersion,
		StorageStatuses: storageStatuses,
	}, nil
}

func shardCheck(shardPath string) (bool, bool) {
	info, err := os.Stat(shardPath)
	if err != nil {
		return false, false
	}

	readable := info.Mode()&(permission.OwnerRead|permission.OwnerExecute) == permission.OwnerRead|permission.OwnerExecute
	writable := info.Mode()&permission.OwnerWrite == permission.OwnerWrite

	return readable, writable
}
