package praefect

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// The changeset log is written by each Gitaly node for itself and is not
// replicated, so reads of it must not be spread across replicas.
func TestChangesetRPCsAreRoutedToThePrimary(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	for _, method := range []string{
		"/gitaly.ChangesetService/ListChangesets",
		"/gitaly.ChangesetService/WatchChangesets",
	} {
		t.Run(method, func(t *testing.T) {
			info, err := protoregistry.GitalyProtoPreregistered.LookupMethod(method)
			require.NoError(t, err)
			require.Equal(t, protoregistry.OpAccessor, info.Operation)

			require.True(t, shouldRouteRepositoryAccessorToPrimary(ctx, grpcCall{fullMethodName: method}))

			incoming := metadata.NewIncomingContext(ctx, metadata.Pairs("some-header", "value"))
			require.True(t, shouldRouteRepositoryAccessorToPrimary(incoming, grpcCall{fullMethodName: method}))
		})
	}

	// Other accessors of the object service stay distributed: objects are the
	// same on every up-to-date replica.
	require.False(t, shouldRouteRepositoryAccessorToPrimary(ctx, grpcCall{fullMethodName: "/gitaly.ObjectService/GetObjects"}))
}
