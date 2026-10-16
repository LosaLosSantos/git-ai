package client

import (
	"context"
	"io"

	"github.com/feder-cr/git-ai/internal/gitaly/gitalyclient"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc"
)

// UploadArchive proxies an SSH git-upload-archive (git archive --remote) session to Gitaly
func UploadArchive(ctx context.Context, conn *grpc.ClientConn, stdin io.Reader, stdout, stderr io.Writer, req *gitalypb.SSHUploadArchiveRequest) (int32, error) {
	return gitalyclient.UploadArchive(ctx, conn, stdin, stdout, stderr, req)
}
