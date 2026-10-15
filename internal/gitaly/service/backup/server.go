package backup

import (
	"github.com/feder-cr/open_scion/internal/backup"
	"github.com/feder-cr/open_scion/internal/gitaly/service"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedBackupServiceServer
	backupSink *backup.Sink
}

// NewServer creates a new instance of a gRPC backup server.
func NewServer(deps *service.Dependencies) gitalypb.BackupServiceServer {
	return &server{
		backupSink: deps.GetBackupSink(),
	}
}
