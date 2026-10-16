package partition

import (
	"github.com/feder-cr/git-ai/internal/backup"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedPartitionServiceServer
	logger     log.Logger
	txManager  transaction.Manager
	node       storage.Node
	backupSink *backup.Sink
}

// NewServer creates a new instance of a gRPC repo server
func NewServer(deps *service.Dependencies) gitalypb.PartitionServiceServer {
	return &server{
		logger:     deps.GetLogger(),
		txManager:  deps.GetTxManager(),
		node:       deps.GetNode(),
		backupSink: deps.GetBackupSink(),
	}
}
