package hook

import (
	"context"
	"io"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// ReceiveReportHook runs after git-receive-pack(1) has updated the references but
// before it reports the push status to the client. A non-zero exit status turns
// every otherwise-successful reference update into a failure, which is the
// window in which a durable object-store publish can reject a push before the
// client has been told that it succeeded.
//
// It is not implemented yet and unconditionally succeeds, so pushes behave
// exactly as they did before the hook existed.
func (m *GitLabHookManager) ReceiveReportHook(context.Context, *gitalypb.Repository, []string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}
