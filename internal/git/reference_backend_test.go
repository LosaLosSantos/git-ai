package git

import (
	"testing"

	"github.com/feder-cr/open_scion/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestReferenceBackendProtoConversion(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc     string
		proto    gitalypb.ReferenceBackend
		internal ReferenceBackend
	}{
		{
			desc:     "unspecified",
			proto:    gitalypb.ReferenceBackend_REFERENCE_BACKEND_UNSPECIFIED,
			internal: ReferenceBackend{},
		},
		{
			desc:     "files",
			proto:    gitalypb.ReferenceBackend_REFERENCE_BACKEND_FILES,
			internal: ReferenceBackendFiles,
		},
		{
			desc:     "reftable",
			proto:    gitalypb.ReferenceBackend_REFERENCE_BACKEND_REFTABLE,
			internal: ReferenceBackendReftables,
		},
		{
			desc:     "mvcc",
			proto:    gitalypb.ReferenceBackend_REFERENCE_BACKEND_MVCC,
			internal: ReferenceBackendMVCC,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			backend, err := ReferenceBackendByProto(tc.proto)
			require.NoError(t, err)
			require.Equal(t, tc.internal, backend)

			// A backend read off one request must round-trip onto another.
			protoBackend, err := ReferenceBackendToProto(backend)
			require.NoError(t, err)
			require.Equal(t, tc.proto, protoBackend)
		})
	}

	t.Run("unknown proto backend", func(t *testing.T) {
		t.Parallel()

		_, err := ReferenceBackendByProto(gitalypb.ReferenceBackend(-1))
		require.Error(t, err)
	})

	t.Run("unknown internal backend", func(t *testing.T) {
		t.Parallel()

		_, err := ReferenceBackendToProto(ReferenceBackend{Name: "nonsense"})
		require.Error(t, err)
	})
}
