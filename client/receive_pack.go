package client

import (
	"context"
	"io"

	"github.com/feder-cr/open_scion/internal/gitaly/gitalyclient"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"google.golang.org/grpc"
)

// ReceivePack proxies an SSH git-receive-pack (git push) session to Gitaly
func ReceivePack(ctx context.Context, conn *grpc.ClientConn, stdin io.Reader, stdout, stderr io.Writer, req *gitalypb.SSHReceivePackRequest) (int32, error) {
	return gitalyclient.ReceivePack(ctx, conn, stdin, stdout, stderr, req)
}
