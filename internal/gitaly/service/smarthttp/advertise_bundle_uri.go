package smarthttp

import (
	"context"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// AdvertiseBundleURI serves the Git protocol v2 `command=bundle-uri` request, which Git sends as
// its own `--stateless-rpc` request before the actual `command=fetch`. Serving it merely makes
// git-upload-pack(1) write back the bundle list from the repository's configuration, which is far
// cheaper than serving a clone or fetch.
//
// The RPC is a thin wrapper around uploadPackWithSidechannel and exists only so that this cheap
// command has a distinct method name, and thus its own concurrency-limiting cost class: concurrency
// limits are keyed by the invoked RPC, so bundle-uri requests would otherwise queue behind clones.
//
// The request is deliberately not inspected to verify that it really carries `command=bundle-uri`.
// Gitaly is only reachable behind authenticated callers such as Workhorse and gitlab-shell, and
// those services are the gatekeepers that decide which RPC to invoke for a given request.
//
// AdvertiseBundleURI force-enables the bundle-uri capability so that git-upload-pack(1) answers the
// command instead of rejecting it.
func (s *server) AdvertiseBundleURI(ctx context.Context, req *gitalypb.AdvertiseBundleURIRequest) (*gitalypb.AdvertiseBundleURIResponse, error) {
	negotiation, err := s.uploadPackWithSidechannel(ctx, &gitalypb.PostUploadPackWithSidechannelRequest{
		Repository:       req.GetRepository(),
		GitConfigOptions: req.GetGitConfigOptions(),
		GitProtocol:      req.GetGitProtocol(),
	}, true)
	if err != nil {
		return nil, err
	}

	return &gitalypb.AdvertiseBundleURIResponse{
		PackfileNegotiationStatistics: negotiation.ToProto(),
	}, nil
}
