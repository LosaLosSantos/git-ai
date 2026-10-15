package hook

import (
	"context"
	"errors"

	"github.com/feder-cr/open_scion/internal/grpc/backchannel"
	"github.com/feder-cr/open_scion/internal/structerr"
	"github.com/feder-cr/open_scion/proto/go/gitalypb"
)

// SignalPostReceiveReady signals that Praefect has completed transaction coordination
// and the PostReceiveHook can proceed with calling Rails. This RPC is only callable via
// Praefect's backchannel to prevent abuse.
func (s *server) SignalPostReceiveReady(
	ctx context.Context,
	req *gitalypb.SignalPostReceiveReadyRequest,
) (*gitalypb.SignalPostReceiveReadyResponse, error) {
	// Validate that the request is coming via Praefect's backchannel.
	// This prevents external callers from unblocking arbitrary transactions.
	if _, err := backchannel.GetPeerID(ctx); err != nil {
		return nil, structerr.NewPermissionDenied("SignalPostReceiveReady can only be called via Praefect backchannel")
	}

	// Validate transaction ID is non-zero to prevent abuse.
	if req.GetTransactionId() == 0 {
		return nil, structerr.NewInvalidArgument("transaction_id must be non-zero")
	}

	if s.postReceiveRegistry == nil {
		return &gitalypb.SignalPostReceiveReadyResponse{}, nil
	}

	var signalErr error
	if req.GetPostTransactionFailed() {
		signalErr = errors.New("Praefect post-transaction work failed")
	}
	s.postReceiveRegistry.Signal(req.GetTransactionId(), signalErr)

	return &gitalypb.SignalPostReceiveReadyResponse{}, nil
}
