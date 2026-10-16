package config

import (
	"encoding/json"
	"testing"

	"github.com/feder-cr/git-ai/internal/errors/cfgerror"
	"github.com/stretchr/testify/require"
)

func TestNode_MarshalJSON(t *testing.T) {
	token := "secretToken"
	node := &Node{
		Storage: "storage",
		Address: "address",
		Token:   token,
	}

	b, err := json.Marshal(node)
	require.NoError(t, err)
	require.JSONEq(t, `{"storage":"storage","address":"address"}`, string(b))
}

func TestNode_Validate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		node        Node
		expectedErr error
	}{
		{
			name: "valid",
			node: Node{Storage: "storage", Address: "address"},
		},
		{
			name: "invalid",
			node: Node{Storage: "", Address: " \n \t"},
			expectedErr: cfgerror.ValidationErrors{
				{
					Key:   []string{"storage"},
					Cause: cfgerror.ErrBlankOrEmpty,
				},
				{
					Key:   []string{"address"},
					Cause: cfgerror.ErrBlankOrEmpty,
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.node.Validate()
			require.Equal(t, tc.expectedErr, err)
		})
	}
}
