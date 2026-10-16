package grant_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/feder-cr/git-ai/proxy/grant"
	"github.com/feder-cr/git-ai/proxy/policy"
	"github.com/stretchr/testify/require"
)

func TestJSONContract(t *testing.T) {
	data, err := os.ReadFile("testdata/orbit.json")
	require.NoError(t, err)

	var value grant.Grant
	require.NoError(t, json.Unmarshal(data, &value))
	require.Equal(t, "tcp://praefect.internal:2305", value.Coordinates.Address)
	require.Equal(t, "gitaly-token", value.Coordinates.Token)
	require.Equal(t, "gkg-indexer", value.Coordinates.CallMetadata["client_name"])
	require.Equal(t, "readonly_repository", value.Authorization.Profile)
	require.Equal(t, "default", value.Authorization.Repository.GetStorageName())
	require.Len(t, value.Authorization.AllowedMethods, 25)
	require.Equal(t, 600, value.Authorization.ExpiresIn)
	require.Equal(t, uint32(32), value.Authorization.MaxConcurrentStreams)
	require.Equal(t, int64(42), value.Authorization.ProjectID)
	require.Equal(t, int64(7), value.Authorization.RootNamespaceID)

	profile, ok := policy.LookupProfile(value.Authorization.Profile)
	require.True(t, ok)
	for _, fullMethod := range value.Authorization.AllowedMethods {
		method, err := policy.CheckUniversalInvariants(fullMethod)
		require.NoError(t, err, fullMethod)
		require.NoError(t, profile.CheckMethod(method), fullMethod)
	}
}
