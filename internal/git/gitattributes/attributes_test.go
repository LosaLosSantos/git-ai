package gitattributes

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttributes_IsSet(t *testing.T) {
	t.Parallel()

	attrs := Attributes{
		Attribute{Name: "truthy", State: Set},
		Attribute{Name: "falsey", State: Unset},
		Attribute{Name: "empty", State: ""},
		Attribute{Name: "foo", State: "bar"},
	}
	for _, tc := range []struct {
		desc     string
		name     string
		expected bool
	}{
		{
			desc:     "truthy value",
			name:     "truthy",
			expected: true,
		},
		{
			desc:     "falsey value",
			name:     "falsey",
			expected: false,
		},
		{
			desc:     "empty value",
			name:     "empty",
			expected: false,
		},
		{
			desc:     "non-bool value",
			name:     "foo",
			expected: false,
		},
		{
			desc:     "non-existing attribute",
			name:     "blurp",
			expected: false,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.expected, attrs.IsSet(tc.name))
		})
	}
}

func TestAttributes_IsUnset(t *testing.T) {
	t.Parallel()

	attrs := Attributes{
		Attribute{Name: "truthy", State: Set},
		Attribute{Name: "falsey", State: Unset},
		Attribute{Name: "empty", State: ""},
		Attribute{Name: "foo", State: "bar"},
	}
	for _, tc := range []struct {
		desc     string
		name     string
		expected bool
	}{
		{
			desc:     "truthy value",
			name:     "truthy",
			expected: false,
		},
		{
			desc:     "falsey value",
			name:     "falsey",
			expected: true,
		},
		{
			desc:     "empty value",
			name:     "empty",
			expected: false,
		},
		{
			desc:     "non-bool value",
			name:     "foo",
			expected: false,
		},
		{
			desc:     "non-existing attribute",
			name:     "blurp",
			expected: false,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.expected, attrs.IsUnset(tc.name))
		})
	}
}

func TestAttributes_StateFor(t *testing.T) {
	t.Parallel()

	attrs := Attributes{
		Attribute{Name: "truthy", State: Set},
		Attribute{Name: "falsey", State: Unset},
		Attribute{Name: "empty", State: ""},
		Attribute{Name: "foo", State: "bar"},
	}
	for _, tc := range []struct {
		desc          string
		name          string
		expectedOk    bool
		expectedState string
	}{
		{
			desc:          "truthy value",
			name:          "truthy",
			expectedOk:    true,
			expectedState: Set,
		},
		{
			desc:          "falsey value",
			name:          "falsey",
			expectedOk:    true,
			expectedState: Unset,
		},
		{
			desc:          "empty value",
			name:          "empty",
			expectedOk:    true,
			expectedState: "",
		},
		{
			desc:          "non-bool value",
			name:          "foo",
			expectedOk:    true,
			expectedState: "bar",
		},
		{
			desc:       "non-existing attribute",
			name:       "blurp",
			expectedOk: false,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			state, ok := attrs.StateFor(tc.name)
			require.Equal(t, tc.expectedOk, ok)
			if ok {
				require.Equal(t, tc.expectedState, state)
			}
		})
	}
}
